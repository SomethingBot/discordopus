package discordopus

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"

	"github.com/kkdai/youtube/v2"
	"layeh.com/gopus"
)

var ErrNoAudioStream = errors.New("no audio streams found")

func GetAudioStream(url string) (io.ReadCloser, error) {
	client := youtube.Client{Debug: false, HTTPClient: http.DefaultClient}

	video, err := client.GetVideo(url)
	if err != nil {
		return nil, fmt.Errorf("could not get video (%w)", err)
	}

	formatList := video.Formats.Type("audio")
	formatList.Sort()

	if len(formatList) == 0 {
		return nil, ErrNoAudioStream
	}

	readCloser, _, err := client.GetStream(video, &formatList[0])
	if err != nil {
		return nil, fmt.Errorf("could not get audio stream (%w)", err)
	}

	return readCloser, nil
}

type MultiCloser []io.Closer

func (mc *MultiCloser) Close() error {
	var err error

	for _, c := range []io.Closer(*mc) {
		err2 := c.Close()
		if err2 != nil {
			err = errors.Join(err, fmt.Errorf("could not close reader (%w)", err2))
		}
	}
	return err
}

type functionCloser func() error

func (fc *functionCloser) Close() error {
	return (func() error)(*fc)()
}

type CloseReader struct {
	io.Reader
	io.Closer
}

const channels = 2
const frameRate = 48000
const frameSize = 960
const maxBytes = (frameSize * channels) * channels

func LiveConvertAudioStreamToS16LE(audioStream io.Reader) (io.ReadCloser, error) {
	/* #nosec G204 */
	ffmpeg := exec.Command("ffmpeg", "-i", "pipe:", "-f", "s16le", "-ar", fmt.Sprint(frameRate), "-ac", fmt.Sprint(channels), "pipe:1")
	ffmpeg.Stdin = audioStream
	ffmpeg.Stderr = os.Stderr

	readCloser, err := ffmpeg.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("could not get StdoutPipe from ffmpeg (%w)", err)
	}

	err = ffmpeg.Start()
	if err != nil {
		return nil, fmt.Errorf("ffmpeg start error (%w)", err)
	}

	fc := functionCloser(ffmpeg.Process.Kill)

	mc := MultiCloser([]io.Closer{&fc, readCloser})

	return &CloseReader{Closer: &mc, Reader: readCloser}, nil
}

type PCMData struct {
	Error error
	Data  []byte
}

// ConvertS16LEBytesToPCM read from reader, and output to channel; must read chan until emptied, and error check on last returned PCMData
func ConvertS16LEBytesToPCM(reader io.Reader) chan PCMData {
	pcmData := make(chan PCMData, 1)
	go func(pcmData chan PCMData) {
		defer close(pcmData)

		enc, err := gopus.NewEncoder(frameRate, channels, gopus.Audio)
		if err != nil {
			pcmData <- PCMData{Error: fmt.Errorf("coud not get new gopus encoder (%w)", err)}
			return
		}

		buf := make([]int16, frameSize*channels)

		var opusData []byte
		for {
			err = binary.Read(reader, binary.LittleEndian, &buf)
			if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
				return
			}
			if err != nil {
				pcmData <- PCMData{Error: fmt.Errorf("could not binary read from input reader while converting to PCM (%w)", err)}
				return
			}
			opusData, err = enc.Encode(buf, frameSize, maxBytes)
			if err != nil {
				pcmData <- PCMData{Error: fmt.Errorf("could not encode opus data (%w)", err)}
				return
			}
			pcmData <- PCMData{Data: opusData}
		}
	}(pcmData)

	return pcmData
}

package discordopus

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"

	"github.com/SomethingBot/multierror"
	"github.com/kkdai/youtube/v2"
	"layeh.com/gopus"
)

func GetAudioStream(url string) (io.ReadCloser, error) {
	client := youtube.Client{}
	video, err := client.GetVideo(url)
	if err != nil {
		return nil, err
	}

	formatList := video.Formats.Type("audio")
	formatList.Sort()

	if len(formatList) == 0 {
		return nil, errors.New("no audio streams found")
	}

	var rc io.ReadCloser
	rc, _, err = client.GetStream(video, &formatList[0])
	if err != nil {
		return nil, err
	}

	return rc, nil
}

type MultiCloser []io.Closer

func (mc *MultiCloser) Close() error {
	var err error
	for _, c := range []io.Closer(*mc) {
		err2 := c.Close()
		if err2 != nil {
			err = multierror.Append(err, err2)
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
const maxBytes = (frameSize * 2) * 2

func LiveConvertAudioStreamToS16LE(audioStream io.Reader) (io.ReadCloser, error) {
	ffmpeg := exec.Command("ffmpeg", "-i", "pipe:", "-f", "s16le", "-ar", fmt.Sprint(frameRate), "-ac", fmt.Sprint(channels), "pipe:1")
	ffmpeg.Stdin = audioStream
	ffmpeg.Stderr = os.Stderr

	readCloser, err := ffmpeg.StdoutPipe()
	if err != nil {
		return nil, err
	}

	err = ffmpeg.Start()
	if err != nil {
		return nil, err
	}

	var fc functionCloser
	fc = ffmpeg.Process.Kill

	mc := MultiCloser([]io.Closer{&fc, readCloser})

	return &CloseReader{Closer: &mc, Reader: readCloser}, err
}

type PCMData struct {
	err  error
	data []byte
}

// ConvertS16LEBytesToPCM read from reader, and output to channel; must read chan until emptied, and error check on last returned PCMData
func ConvertS16LEBytesToPCM(reader io.Reader) chan PCMData {
	pcmData := make(chan PCMData, 1)
	go func(pcmData chan PCMData) {
		defer close(pcmData)

		enc, err := gopus.NewEncoder(frameRate, channels, gopus.Audio)
		if err != nil {
			pcmData <- PCMData{err: err}
			return
		}

		buf := make([]int16, frameSize*channels)
		var opusData []byte
		for {
			err = binary.Read(reader, binary.LittleEndian, &buf)
			if err == io.EOF || err == io.ErrUnexpectedEOF {
				return
			}
			if err != nil {
				pcmData <- PCMData{err: err}
				return
			}
			opusData, err = enc.Encode(buf, frameSize, maxBytes)
			if err != nil {
				pcmData <- PCMData{err: err}
				return
			}
			pcmData <- PCMData{data: opusData}
		}
	}(pcmData)
	return pcmData
}

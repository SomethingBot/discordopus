package discordopus

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"unsafe"

	"github.com/ebitengine/purego"
	"github.com/kkdai/youtube/v2"
)

var ErrNoAudioStream = errors.New("no audio streams found")

func GetAudioStream(url string) (io.ReadCloser, error) {
	client := youtube.Client{HTTPClient: http.DefaultClient}

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
	var (
		err  error
		err2 error
	)
	for _, c := range []io.Closer(*mc) {
		err2 = c.Close()
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

const (
	channels  = 2
	frameRate = 48000
	frameSize = 960
	maxBytes  = (frameSize * channels) * channels
)

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

	return &CloseReader{
		Closer: &mc,
		Reader: readCloser,
	}, nil
}

type OpusData struct {
	Error error
	Data  []byte
}

func ConvertS16LEBytesToOpusBytes(reader io.Reader) chan OpusData {
	opusData := make(chan OpusData, 1)

	libopus, err := purego.Dlopen("libopus.so.0", purego.RTLD_NOW|purego.RTLD_GLOBAL)
	if err != nil {
		opusData <- OpusData{Error: fmt.Errorf("could not open libopusenc.so.0 error (%w)", err)}
		return opusData
	}

	var opusEncGetSize func(channels int) int
	purego.RegisterLibFunc(&opusEncGetSize, libopus, "opus_encoder_get_size")

	data := make([]byte, opusEncGetSize(channels)) // Note get size can return 0
	enc := unsafe.Pointer(&data[0])

	var opusEncInit func(encoder unsafe.Pointer, sampleRate int32, channels int, application int) int
	purego.RegisterLibFunc(&opusEncInit, libopus, "opus_encoder_init")

	const opus_application_audio = 2049 // TODO: should this be the VoIP constant?
	encErr := opusEncInit(enc, frameRate, channels, opus_application_audio)
	if encErr != 0 { // TODO: handle errors better
		opusData <- OpusData{Error: fmt.Errorf("could not inital libopus encoder (%v)", encErr)}
		return opusData
	}

	var opusEncode func(encoder unsafe.Pointer, pcm unsafe.Pointer, frameSize int, data unsafe.Pointer, maxBytes int32) int32
	purego.RegisterLibFunc(&opusEncode, libopus, "opus_encode")

	go func() {
		defer close(opusData)

		buf := make([]int16, frameSize*channels)

		for {
			log.Println("called")
			err := binary.Read(reader, binary.LittleEndian, &buf)
			switch {
			case errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF):
				log.Println("io.EOF")
				log.Println(err)
				return
			case err != nil:
				opusData <- OpusData{Error: fmt.Errorf("could not binary read from input reader while encoding opus (%w)", err)}
				return
			}

			pcmPtr := unsafe.Pointer(&buf[0])
			encData := make([]byte, maxBytes)
			encDataPtr := unsafe.Pointer(&encData[0])

			encodeN := opusEncode(enc, pcmPtr, frameSize, encDataPtr, int32(len(encData)))
			encode := int(encodeN)
			log.Printf("encode: (%v) (%v)", encode, encodeN)

			if encode < 0 {
				opusData <- OpusData{Error: fmt.Errorf("could not encode to opus error (%v)", encode)}
				return
			}

			opusData <- OpusData{Data: encData[0:encode]}
		}
	}()

	return opusData
}

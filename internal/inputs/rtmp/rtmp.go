package rtmp

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"image"
	"image/jpeg"
	"io"
	"net"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/Glimesh/go-fdkaac/fdkaac"
	"github.com/Glimesh/waveguide/pkg/control"
	"github.com/Glimesh/waveguide/pkg/h264"
	h264joy "github.com/nareix/joy5/codec/h264"
	"github.com/pion/rtp"
	"github.com/pion/rtp/codecs"
	"github.com/pion/webrtc/v3"
	"github.com/sirupsen/logrus"
	flvtag "github.com/yutopp/go-flv/tag"
	gortmp "github.com/yutopp/go-rtmp"
	rtmpmsg "github.com/yutopp/go-rtmp/message"
	opus "gopkg.in/hraban/opus.v2"
)

const (
	FTL_MTU      uint16 = 1392
	FTL_VIDEO_PT        = 96
	FTL_AUDIO_PT        = 97

	BANDWIDTH_LIMIT = 8000 * 1000
)

type RTMPSource struct {
	log     logrus.FieldLogger
	config  RTMPSourceConfig
	control *control.Control
}

type RTMPSourceConfig struct {
	// Listen address of the RTMP server in the ip:port format
	Address string
}

func New(config RTMPSourceConfig) *RTMPSource {
	return &RTMPSource{
		config: config,
	}
}

func (s *RTMPSource) SetControl(ctrl *control.Control) {
	s.control = ctrl
}

func (s *RTMPSource) SetLogger(log logrus.FieldLogger) {
	s.log = log
}

func (s *RTMPSource) Listen(ctx context.Context) {
	tcpAddr, err := net.ResolveTCPAddr("tcp", s.config.Address)
	if err != nil {
		s.log.Errorf("Failed: %+v", err)
	}

	listener, err := net.ListenTCP("tcp", tcpAddr)
	if err != nil {
		s.log.Errorf("Failed: %+v", err)
	}

	s.log.Infof("Starting RTMP Server on %s", s.config.Address)

	srv := gortmp.NewServer(&gortmp.ServerConfig{
		OnConnect: func(conn net.Conn) (io.ReadWriteCloser, *gortmp.ConnConfig) {
			return conn, &gortmp.ConnConfig{
				Handler: &connHandler{
					control:                s.control,
					log:                    s.log,
					stopMetadataCollection: make(chan bool, 1),
				},

				ControlState: gortmp.StreamControlStateConfig{
					DefaultBandwidthWindowSize: 6 * 1024 * 1024 / 8,
				},
				Logger: s.log.WithField("app", "yutopp/go-rtmp"),
			}
		},
	})
	if err := srv.Serve(listener); err != nil {
		s.log.Panicf("Failed: %+v", err)
	}
}

type connHandler struct {
	gortmp.DefaultHandler
	control *control.Control

	log logrus.FieldLogger

	channelID        control.ChannelID
	streamID         control.StreamID
	streamKey        []byte
	started          bool
	authenticated    bool
	errored          bool
	metadataFailures int

	stream *control.Stream

	videoTrack *webrtc.TrackLocalStaticRTP
	audioTrack *webrtc.TrackLocalStaticRTP

	videoSequencer  rtp.Sequencer
	videoPacketizer rtp.Packetizer
	videoClockRate  uint32

	audioSequencer  rtp.Sequencer
	audioPacketizer rtp.Packetizer
	audioClockRate  uint32
	audioDecoder    *fdkaac.AacDecoder
	audioBuffer     []byte
	audioEncoder    *opus.Encoder

	keyframes       int
	lastKeyFrames   int
	lastInterFrames int

	sps []byte
	pps []byte

	stopMetadataCollection chan bool

	// Metadata
	startTime           int64
	lastTime            int64 // Last time the metadata collector ran
	audioBps            int
	videoBps            int
	audioPackets        int
	videoPackets        int
	lastAudioPackets    int
	lastVideoPackets    int
	clientVendorName    string
	clientVendorVersion string
	videoCodec          string
	audioCodec          string
	videoHeight         int
	videoWidth          int

	outputBytes int

	debugSaveVideo bool
	debugVideoFile *os.File
	lastFullFrame  []byte

	videoJoyCodec *h264joy.Codec
}

func (h *connHandler) OnServe(conn *gortmp.Conn) {
	h.log.Info("OnServe: %#v", conn)
}

func (h *connHandler) OnConnect(timestamp uint32, cmd *rtmpmsg.NetConnectionConnect) (err error) {
	h.log.Info("OnConnect: %#v", cmd)

	h.metadataFailures = 0
	h.errored = false

	h.videoClockRate = 90000
	// TODO: This can be customized by the user, we should figure out how to infer it from the client
	h.audioClockRate = 48000

	h.startTime = time.Now().Unix()
	h.audioCodec = "opus"
	h.videoCodec = "H264"
	h.videoHeight = 0
	h.videoWidth = 0

	return nil
}

func (h *connHandler) OnCreateStream(timestamp uint32, cmd *rtmpmsg.NetConnectionCreateStream) error {
	h.log.Info("OnCreateStream: %#v", cmd)
	return nil
}

func (h *connHandler) OnPublish(ctx *gortmp.StreamContext, timestamp uint32, cmd *rtmpmsg.NetStreamPublish) (err error) {
	h.log.Info("OnPublish: %#v", cmd)

	if cmd.PublishingName == "" {
		return errors.New("PublishingName is empty")
	}
	// Authenticate
	auth := strings.SplitN(cmd.PublishingName, "-", 2)
	u64, err := strconv.ParseUint(auth[0], 10, 32)

	if err != nil {
		h.log.Error(err)
		return err
	}
	h.channelID = control.ChannelID(u64)
	h.streamKey = []byte(auth[1])

	h.started = true

	if err := h.control.Authenticate(h.channelID, h.streamKey); err != nil {
		h.log.Error(err)
		return err
	}

	stream, err := h.control.StartStream(h.channelID)
	if err != nil {
		h.log.Error(err)
		return err
	}

	h.authenticated = true

	h.stream = stream
	h.streamID = stream.StreamID

	// Add some meta info to the logger
	h.log = h.log.WithFields(logrus.Fields{
		"channel_id": h.channelID,
		"stream_id":  h.streamID,
	})

	if err := h.initVideo(h.videoClockRate); err != nil {
		return err
	}
	if err := h.initAudio(h.audioClockRate); err != nil {
		return err
	}

	h.control.AddTrack(h.channelID, h.videoTrack)
	h.control.AddTrack(h.channelID, h.audioTrack)

	go h.setupMetadataCollector()

	return nil
}

func (h *connHandler) OnClose() {
	h.log.Info("OnClose")

	h.stopMetadataCollection <- true

	// We only want to publish the stop if it's ours
	if h.authenticated {
		// StopStream mainly calls external services, there's a chance this call can hang for a bit while the other services are processing
		// However it's not safe to call RemoveStream until this is finished or the pointer wont... exist?
		if err := h.control.StopStream(h.channelID); err != nil {
			h.log.Error(err)
			// panic(err)
		}
	}
	h.authenticated = false

	h.started = false

	if h.audioDecoder != nil {
		h.audioDecoder.Close()
		h.audioDecoder = nil
	}

	if h.debugSaveVideo {
		h.debugVideoFile.Close()
	}
}

func (h *connHandler) initAudio(clockRate uint32) (err error) {
	h.audioSequencer = rtp.NewFixedSequencer(0) // ftl client says this should be changed to a random value
	h.audioPacketizer = rtp.NewPacketizer(FTL_MTU, FTL_AUDIO_PT, uint32(h.channelID), &codecs.OpusPayloader{}, h.audioSequencer, clockRate)

	h.audioTrack, err = webrtc.NewTrackLocalStaticRTP(webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeOpus}, "audio", "pion")
	if err != nil {
		return err
	}

	h.audioEncoder, err = opus.NewEncoder(int(clockRate), 2, opus.AppAudio)
	if err != nil {
		return err
	}
	h.audioDecoder = fdkaac.NewAacDecoder()

	return nil
}

func (h *connHandler) OnAudio(timestamp uint32, payload io.Reader) error {
	if h.errored {
		return errors.New("stream is not longer authenticated")
	}

	// Convert AAC to opus
	var audio flvtag.AudioData
	if err := flvtag.DecodeAudioData(payload, &audio); err != nil {
		return err
	}

	data, err := io.ReadAll(audio.Data)
	if err != nil {
		return err
	}

	if audio.AACPacketType == flvtag.AACPacketTypeSequenceHeader {
		h.log.Infof("Created new codec %s", hex.EncodeToString(data))
		err := h.audioDecoder.InitRaw(data)

		if err != nil {
			h.log.WithError(err).Errorf("error initializing stream")
			return fmt.Errorf("can't initialize codec with %s", hex.EncodeToString(data))
		}

		return nil
	}

	pcm, err := h.audioDecoder.Decode(data)
	if err != nil {
		h.log.Errorf("decode error: %s %s", hex.EncodeToString(data), err)
		return fmt.Errorf("decode error")
	}

	blockSize := 960
	for h.audioBuffer = append(h.audioBuffer, pcm...); len(h.audioBuffer) >= blockSize*4; h.audioBuffer = h.audioBuffer[blockSize*4:] {
		pcm16 := make([]int16, blockSize*2)
		for i := 0; i < len(pcm16); i++ {
			pcm16[i] = int16(binary.LittleEndian.Uint16(h.audioBuffer[i*2:]))
		}
		bufferSize := 1024
		opusData := make([]byte, bufferSize)
		n, err := h.audioEncoder.Encode(pcm16, opusData)
		if err != nil {
			return err
		}
		opusOutput := opusData[:n]

		packets := h.audioPacketizer.Packetize(opusOutput, uint32(blockSize))

		for _, p := range packets {
			h.audioPackets++
			h.outputBytes += len(p.Payload)

			if err := h.audioTrack.WriteRTP(p); err != nil {
				return err
			}
		}
	}

	return nil
}

func (h *connHandler) initVideo(clockRate uint32) (err error) {
	h.videoSequencer = rtp.NewFixedSequencer(25000)
	h.videoPacketizer = rtp.NewPacketizer(FTL_MTU, FTL_VIDEO_PT, uint32(h.channelID+1), &codecs.H264Payloader{}, h.videoSequencer, clockRate)

	h.videoTrack, err = webrtc.NewTrackLocalStaticRTP(webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeH264}, "video", "pion")
	if err != nil {
		return err
	}

	if h.debugSaveVideo {
		h.debugVideoFile, err = os.Create(fmt.Sprintf("debug-video-%d.h264", h.streamID))
		return err
	}

	return nil
}

func (h *connHandler) OnVideo(timestamp uint32, payload io.Reader) error {
	if h.errored {
		return errors.New("stream is not longer authenticated")
	}

	var video flvtag.VideoData
	if err := flvtag.DecodeVideoData(payload, &video); err != nil {
		return err
	}

	// video.CodecID == H264, I wonder if we should check this?
	// video.FrameType does not seem to contain b-frames even if they exist

	switch video.FrameType {
	case flvtag.FrameTypeKeyFrame:
		h.lastKeyFrames += 1
		h.keyframes += 1
	case flvtag.FrameTypeInterFrame:
		h.lastInterFrames += 1
	default:
		h.log.Debug("Unknown FLV Video Frame: %+v\n", video)
	}

	data, err := io.ReadAll(video.Data)
	if err != nil {
		return err
	}

	// From: https://github.com/nareix/joy5/blob/2c912ca30590ee653145d93873b0952716d21093/cmd/avtool/seqhdr.go#L38-L65
	// joy5 is an unlicensed project -- need to confirm usage.
	// Look at video.AVCPacketType == flvtag.AVCPacketTypeSequenceHeader to figure out sps and pps
	// Store those in the stream object, then use them later for the keyframes
	if video.AVCPacketType == flvtag.AVCPacketTypeSequenceHeader {
		h.videoJoyCodec, err = h264joy.FromDecoderConfig(data)
		if err != nil {
			return err
		}
	}

	var outBuf []byte
	if video.FrameType == flvtag.FrameTypeKeyFrame {
		pktnalus, _ := h264joy.SplitNALUs(data)
		nalus := [][]byte{}
		nalus = append(nalus, h264joy.Map2arr(h.videoJoyCodec.SPS)...)
		nalus = append(nalus, h264joy.Map2arr(h.videoJoyCodec.PPS)...)
		nalus = append(nalus, pktnalus...)
		data := h264joy.JoinNALUsAnnexb(nalus)
		outBuf = data
	} else {
		pktnalus, _ := h264joy.SplitNALUs(data)
		data := h264joy.JoinNALUsAnnexb(pktnalus)
		outBuf = data
	}

	h.debugVideoFile.Write(outBuf)

	if video.FrameType == flvtag.FrameTypeKeyFrame {
		// Save the last full keyframe for anything we may need, eg thumbnails
		h.lastFullFrame = outBuf
	}

	// Likely there's more than one set of RTP packets in this read
	samples := uint32(len(outBuf)) + h.videoClockRate
	packets := h.videoPacketizer.Packetize(outBuf, samples)

	for _, p := range packets {
		h.videoPackets++
		h.outputBytes += len(p.Payload)

		if err := h.videoTrack.WriteRTP(p); err != nil {
			return err
		}
	}

	return nil
}

func (h *connHandler) sendThumbnail() {
	var img image.Image
	h264dec, err := h264.NewH264Decoder()
	if err != nil {
		h.log.Error(err)
		return
	}
	defer h264dec.Close()
	img, err = h264dec.Decode(h.lastFullFrame)
	if err != nil {
		h.log.Error(err)
		return
	}

	if img != nil {
		buff := new(bytes.Buffer)
		err = jpeg.Encode(buff, img, &jpeg.Options{
			Quality: 75,
		})
		if err != nil {
			h.log.Error(err)
			return
		}

		err = h.control.SendThumbnail()
		if err != nil {
			h.log.Error(err)
		}
		buff.Reset()

		// Also update our metadata
		h.videoWidth = img.Bounds().Dx()
		h.videoHeight = img.Bounds().Dy()
	}
}
func (h *connHandler) sendMetadata() {
	// h.streamID, services.StreamMetadata{
	// AudioCodec:        h.audioCodec,
	// IngestServer:      h.manager.orchestrator.ClientHostname,
	// IngestViewers:     0,
	// LostPackets:       0, // Don't exist
	// NackPackets:       0, // Don't exist
	// RecvPackets:       h.videoPackets + h.audioPackets,
	// SourceBitrate:     0, // Likely just need to calculate the bytes between two 5s snapshots?
	// SourcePing:        0, // Not accessible unless we ping them manually
	// StreamTimeSeconds: int(h.lastTime - h.startTime),
	// VendorName:        h.clientVendorName,
	// VendorVersion:     h.clientVendorVersion,
	// VideoCodec:        h.videoCodec,
	// VideoHeight:       h.videoHeight,
	// VideoWidth:        h.videoWidth,
	// }
	err := h.control.SendMetadata()
	if err != nil {
		h.log.Error(err)
	}
}

func (h *connHandler) setupMetadataCollector() {
	ticker := time.NewTicker(5 * time.Second)
	go func() {
		for {
			select {
			case <-ticker.C:
				h.lastTime = time.Now().Unix()

				h.log.WithFields(logrus.Fields{
					"keyframes":   h.lastKeyFrames,
					"interframes": h.lastInterFrames,
					"packets":     h.videoPackets - h.lastVideoPackets,
					"bytes":       h.outputBytes,
				}).Debug("Processed 5s of input frames from RTMP input")

				// Check to ensure we're not over our bandwidth limit
				if h.outputBytes >= BANDWIDTH_LIMIT {
					h.log.Errorf("Sent %d bytes over the last 5 seconds, ending stream", h.outputBytes)
					h.errored = true
				}
				h.outputBytes = 0

				// Calculate some of our last fields
				h.audioBps = 0

				h.lastVideoPackets = h.videoPackets
				h.lastKeyFrames = 0
				h.lastInterFrames = 0

				if len(h.lastFullFrame) > 0 {
					// Todo: Handle thumbnail failures
					h.sendThumbnail()
				}

				err := h.control.SendMetadata()
				if err != nil {
					// Unauthenticate us so the next Video / Audio packet can stop the stream
					h.metadataFailures += 1
					if h.metadataFailures > 5 {
						h.errored = true
						h.log.Error("Metadata failures exceed 5, terminating the stream")
					}

					h.log.Warn(err)
				}
				h.metadataFailures = 0

			case <-h.stopMetadataCollection:
				ticker.Stop()
				return
			}
		}
	}()
}
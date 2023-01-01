package ftl

import (
	"bufio"
	"bytes"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha512"
	"encoding/hex"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"

	"github.com/pion/rtp"
	"github.com/pion/webrtc/v3"
	"github.com/pkg/errors"
	"github.com/sirupsen/logrus"
)

const (
	// This packet size is ideal for super fast video, but the theory is there's
	// going to be numerous problems with networking equipment not accepting the
	// packet size, especially for UDP.
	//packetMtu = 2048 // 67 ms -- 32 frames on 240 video -- 213ms on clock
	// I'm guessing for these two, the packet differences are not great enough to
	// overcome the 30/60 fps most people stream at. So you see no difference.
	//packetMtu = 1600 // 100 ms -- 30 frames on 240 video -- UDP MTU allegedly
	//packetMtu = 1500 // 100 ms gtg latency - 144ms on clock
	//packetMtu = 1460 // UDP MTU
	// packetMtu = 1392
	packetMtu = 1600
	// FTL-SDK recommends 1392 MTU
)

type ConnConfig struct {
	Handler Handler
}

type Handler interface {
	// OnServe(conn *Conn)
	// OnConnect(timestamp uint32, cmd *message.NetConnectionConnect) error
	// OnCreateStream(timestamp uint32, cmd *message.NetConnectionCreateStream) error
	// OnReleaseStream(timestamp uint32, cmd *message.NetConnectionReleaseStream) error
	// OnDeleteStream(timestamp uint32, cmd *message.NetStreamDeleteStream) error
	// OnPublish(ctx *StreamContext, timestamp uint32, cmd *message.NetStreamPublish) error
	// OnPlay(ctx *StreamContext, timestamp uint32, cmd *message.NetStreamPlay) error
	// OnFCPublish(timestamp uint32, cmd *message.NetStreamFCPublish) error
	// OnFCUnpublish(timestamp uint32, cmd *message.NetStreamFCUnpublish) error
	// OnSetDataFrame(timestamp uint32, data *message.NetStreamSetDataFrame) error
	// OnAudio(timestamp uint32, payload io.Reader) error
	// OnVideo(timestamp uint32, payload io.Reader) error
	// OnUnknownMessage(timestamp uint32, msg message.Message) error
	// OnUnknownCommandMessage(timestamp uint32, cmd *message.CommandMessage) error
	// OnUnknownDataMessage(timestamp uint32, data *message.DataMessage) error

	GetHmacKey() (string, error)

	OnConnect(ChannelID) error
	OnPlay() error
	OnTracks(video *webrtc.TrackLocalStaticRTP, audio *webrtc.TrackLocalStaticRTP) error
	OnClose()
}

type ServerConfig struct {
	Log       logrus.FieldLogger
	OnConnect func(net.Conn) (io.ReadWriteCloser, *ConnConfig)
}

func NewServer(config *ServerConfig) *Server {
	return &Server{
		config: config,
		log:    config.Log,
	}
}

type Server struct {
	config *ServerConfig
	log    logrus.FieldLogger

	listener net.Listener
	// mu       sync.Mutex
	// doneCh   chan struct{}
}

func (srv *Server) Serve(listener net.Listener) error {
	srv.listener = listener

	for {
		// Each client
		socket, err := listener.Accept()

		_, connConfig := srv.config.OnConnect(socket)

		ftlConn := FtlConnection{
			log:            srv.log,
			transport:      socket,
			handler:        connConfig.Handler,
			connected:      true,
			mediaConnected: false,
			Metadata:       &FtlConnectionMetadata{},
		}

		if err != nil {
			return err
		}

		go func() {
			for {
				if err := ftlConn.eternalRead(); err != nil {
					ftlConn.log.Error(err)
					ftlConn.Close()
					return
				}
			}
		}()
	}
}

type FtlConnection struct {
	log logrus.FieldLogger

	transport      net.Conn
	mediaTransport *net.UDPConn
	connected      bool
	mediaConnected bool

	handler Handler

	// Unique Channel ID
	channelID int
	//streamKey         string
	assignedMediaPort int

	// Pre-calculated hash we expect the client to return
	hmacPayload []byte
	// Hash the client has actually returned
	clientHmacHash []byte

	hasAuthenticated bool
	hmacRequested    bool

	Metadata *FtlConnectionMetadata

	videoTrack *webrtc.TrackLocalStaticRTP
	audioTrack *webrtc.TrackLocalStaticRTP
}

type FtlConnectionMetadata struct {
	ProtocolVersion string
	VendorName      string
	VendorVersion   string

	HasVideo         bool
	VideoCodec       string
	VideoHeight      uint
	VideoWidth       uint
	VideoPayloadType uint8
	VideoIngestSsrc  uint

	HasAudio         bool
	AudioCodec       string
	AudioPayloadType uint8
	AudioIngestSsrc  uint
}

func (conn *FtlConnection) eternalRead() error {
	// A previous read could have disconnected us already
	if !conn.connected {
		return ErrClosed
	}

	scanner := bufio.NewScanner(conn.transport)
	scanner.Split(scanCRLF)

	for scanner.Scan() {
		payload := scanner.Text()

		// I'm not processing something correctly here
		if payload == "" {
			continue
		}

		if err := conn.ProcessCommand(payload); err != nil {
			return err
		}
	}
	if err := scanner.Err(); err != nil {
		conn.log.Printf("Invalid input: %s", err)
	}

	return nil
}

func (conn *FtlConnection) SendMessage(message string) error {
	message = message + "\n"
	conn.log.Printf("SEND: %q", message)
	_, err := conn.transport.Write([]byte(message))
	return err
}

func (conn *FtlConnection) Close() error {
	err := conn.transport.Close()
	conn.connected = false

	if conn.mediaConnected {
		conn.mediaTransport.Close()
		conn.mediaConnected = false
	}

	conn.handler.OnClose()

	return err
}

func (conn *FtlConnection) ProcessCommand(command string) error {
	conn.log.Printf("RECV: %q", command)
	if command == "HMAC" {
		return conn.processHmacCommand()
	} else if strings.Contains(command, "DISCONNECT") {
		return conn.processDisconnectCommand(command)
	} else if strings.Contains(command, "CONNECT") {
		return conn.processConnectCommand(command)
	} else if strings.Contains(command, "PING") {
		return conn.processPingCommand()
	} else if attributeRegex.MatchString(command) {
		return conn.processAttributeCommand(command)
	} else if command == "." {
		return conn.processDotCommand()
	} else {
		conn.log.Printf("Unknown ingest command: %s", command)
	}
	return nil
}

func (conn *FtlConnection) processHmacCommand() error {
	// hmacKey, err := conn.handler.GetHmacKey()
	// if err != nil {
	// 	return err
	// }
	conn.hmacPayload = make([]byte, hmacPayloadSize)
	rand.Read(conn.hmacPayload)

	encodedPayload := hex.EncodeToString(conn.hmacPayload)

	return conn.SendMessage(fmt.Sprintf(responseHmacPayload, encodedPayload))
}

func (conn *FtlConnection) processDisconnectCommand(message string) error {
	conn.log.Println("Got Disconnect command, closing stuff.")

	return conn.Close()
}

func (conn *FtlConnection) processConnectCommand(message string) error {
	if conn.hmacRequested {
		return ErrMultipleConnect
	}

	conn.hmacRequested = true

	matches := connectRegex.FindAllStringSubmatch(message, 3)
	if len(matches) < 1 {
		return ErrUnexpectedArguments
	}
	args := matches[0]
	if len(args) < 3 {
		// malformed connection string
		return ErrUnexpectedArguments
	}

	channelIdStr := args[1]
	hmacHashStr := args[2]

	channelId, err := strconv.Atoi(channelIdStr)
	if err != nil {
		return ErrUnexpectedArguments
	}

	conn.channelID = channelId

	if err := conn.handler.OnConnect(ChannelID(conn.channelID)); err != nil {
		return err
	}

	hmacKey, err := conn.handler.GetHmacKey()
	if err != nil {
		return err
	}

	hash := hmac.New(sha512.New, []byte(hmacKey))
	hash.Write(conn.hmacPayload)
	conn.hmacPayload = hash.Sum(nil)

	hmacBytes, err := hex.DecodeString(hmacHashStr)
	if err != nil {
		return ErrInvalidHmacHex
	}

	conn.hasAuthenticated = true
	conn.clientHmacHash = hmacBytes

	if !hmac.Equal(conn.clientHmacHash, conn.hmacPayload) {
		return ErrInvalidHmacHash
	}

	return conn.SendMessage(responseOk)
}

func (conn *FtlConnection) processAttributeCommand(message string) error {
	if !conn.hasAuthenticated {
		return ErrConnectBeforeAuth
	}

	matches := attributeRegex.FindAllStringSubmatch(message, 3)
	if len(matches) < 1 || len(matches[0]) < 3 {
		return ErrUnexpectedArguments
	}
	key, value := matches[0][1], matches[0][2]

	switch key {
	case "ProtocolVersion":
		conn.Metadata.ProtocolVersion = value
	case "VendorName":
		conn.Metadata.VendorName = value
	case "VendorVersion":
		conn.Metadata.VendorVersion = value
	// Video
	case "Video":
		conn.Metadata.HasVideo = parseAttributeToBool(value)
	case "VideoCodec":
		conn.Metadata.VideoCodec = value
	case "VideoHeight":
		conn.Metadata.VideoHeight = parseAttributeToUint(value)
	case "VideoWidth":
		conn.Metadata.VideoWidth = parseAttributeToUint(value)
	case "VideoPayloadType":
		conn.Metadata.VideoPayloadType = parseAttributeToUint8(value)
	case "VideoIngestSSRC":
		conn.Metadata.VideoIngestSsrc = parseAttributeToUint(value)
	// Audio
	case "Audio":
		conn.Metadata.HasAudio = parseAttributeToBool(value)
	case "AudioCodec":
		conn.Metadata.AudioCodec = value
	case "AudioPayloadType":
		conn.Metadata.AudioPayloadType = parseAttributeToUint8(value)
	case "AudioIngestSSRC":
		conn.Metadata.AudioIngestSsrc = parseAttributeToUint(value)
	default:
		conn.log.Printf("Unexpected Attribute: %q\n", message)
	}

	return nil
}

func parseAttributeToUint(input string) uint {
	u, _ := strconv.ParseUint(input, 10, 32)
	return uint(u)
}
func parseAttributeToUint8(input string) uint8 {
	u, _ := strconv.ParseUint(input, 10, 32)
	return uint8(u)
}
func parseAttributeToBool(input string) bool {
	return input == "true"
}

func (conn *FtlConnection) processDotCommand() error {
	if !conn.hasAuthenticated {
		return ErrConnectBeforeAuth
	}

	err := conn.listenForMedia()
	if err != nil {
		return err
	}

	// Push it to a clients map so we can reference it later
	if err := conn.handler.OnPlay(); err != nil {
		return err
	}

	return conn.SendMessage(fmt.Sprintf(responseMediaPort, conn.assignedMediaPort))
}

func (conn *FtlConnection) processPingCommand() error {
	return conn.SendMessage(responsePong)
}

func dropCR(data []byte) []byte {
	if len(data) > 0 && data[len(data)-1] == '\r' {
		return data[0 : len(data)-1]
	}
	return data
}

func scanCRLF(data []byte, atEOF bool) (advance int, token []byte, err error) {
	if atEOF && len(data) == 0 {
		return 0, nil, nil
	}
	if i := bytes.Index(data, []byte{'\r', '\n'}); i >= 0 {
		// We have a full newline-terminated line.
		return i + 2, dropCR(data[0:i]), nil
	}
	// If we're at EOF, we have a final, non-terminated line. Return it.
	if atEOF {
		return len(data), dropCR(data), nil
	}
	// Request more data.
	return 0, nil, nil
}

func (conn *FtlConnection) listenForMedia() error {
	udpAddr, err := net.ResolveUDPAddr("udp", ":0")
	if err != nil {
		return err
	}
	mediaConn, mediaErr := net.ListenUDP("udp", udpAddr)
	if mediaErr != nil {
		return err
	}

	conn.assignedMediaPort = mediaConn.LocalAddr().(*net.UDPAddr).Port
	conn.mediaTransport = mediaConn
	conn.mediaConnected = true

	err = conn.createMediaTracks()
	if err != nil {
		conn.Close()
		return err
	}

	if err := conn.handler.OnTracks(conn.videoTrack, conn.audioTrack); err != nil {
		return err
	}

	conn.log.Printf("Listening for UDP connections on: %d", conn.assignedMediaPort)

	go func() {
		for {
			if err := conn.eternalMediaRead(); err != nil {
				conn.log.Error(err)
				conn.Close()
				return
			}
		}
	}()

	return nil
}

// Honestly this function should be refactored into something on OnVideo, OnAudio
// so the library isn't coupled to RTP, but for now this is super fast.
func (conn *FtlConnection) createMediaTracks() error {
	var err error

	// Create a video track
	conn.videoTrack, err = webrtc.NewTrackLocalStaticRTP(webrtc.RTPCodecCapability{MimeType: "video/h264"}, "video", "pion")
	if err != nil {
		return err
	}

	// Create an audio track
	conn.audioTrack, err = webrtc.NewTrackLocalStaticRTP(webrtc.RTPCodecCapability{MimeType: "audio/opus"}, "audio", "pion")
	if err != nil {
		return err
	}

	return nil
}

func (conn *FtlConnection) eternalMediaRead() error {
	if !conn.mediaConnected {
		return ErrClosed
	}

	inboundRTPPacket := make([]byte, packetMtu)

	n, _, err := conn.mediaTransport.ReadFrom(inboundRTPPacket)
	if err != nil {
		return errors.Wrap(ErrRead, err.Error())
	}
	packet := &rtp.Packet{}
	if err = packet.Unmarshal(inboundRTPPacket[:n]); err != nil {
		// fmt.Printf("Error unmarshaling RTP packet %s\n", err)
		return errors.Wrap(ErrRead, err.Error())
	}

	// The FTL client actually tells us what PayloadType to use for these: VideoPayloadType & AudioPayloadType
	if packet.Header.PayloadType == conn.Metadata.VideoPayloadType {
		if err := conn.videoTrack.WriteRTP(packet); err != nil {
			return errors.Wrap(ErrWrite, err.Error())
		}
		// conn.readVideoBytes = conn.readVideoBytes + n
	} else if packet.Header.PayloadType == conn.Metadata.AudioPayloadType {
		if err := conn.audioTrack.WriteRTP(packet); err != nil {
			return errors.Wrap(ErrWrite, err.Error())
		}
		// conn.readAudioBytes = conn.readAudioBytes + n
	}

	return nil
}
package whep

import (
	"context"
	"fmt"
	"html/template"
	"io/ioutil"
	"net/http"
	"strings"

	"github.com/Glimesh/waveguide/pkg/control"
	"github.com/google/uuid"
	"github.com/pion/webrtc/v3"
	"github.com/sirupsen/logrus"
)

type WHEPConfig struct {
	// Listen address of the webserver
	Address string
}

type WHEPServer struct {
	log     logrus.FieldLogger
	config  WHEPConfig
	control *control.Control
}

func New(config WHEPConfig) *WHEPServer {
	return &WHEPServer{
		config: config,
	}
}

func (s *WHEPServer) SetControl(ctrl *control.Control) {
	s.control = ctrl
}

func (s *WHEPServer) SetLogger(log logrus.FieldLogger) {
	s.log = log
}

func (s *WHEPServer) Listen(ctx context.Context) {
	s.log.Infof("Starting WHEP Server on %s", s.config.Address)

	peerConnections := make(map[string]*webrtc.PeerConnection)

	// Todo: Find better way of fetching this path
	streamTemplate := template.Must(template.ParseFiles("internal/outputs/whep/public/stream.html"))

	// Player (Nothing) => Endpoint (Offer) => Player (Answer)
	http.HandleFunc("/whep/endpoint", func(w http.ResponseWriter, r *http.Request) {
		// TODO: Make this handle any Channel ID
		// streamID := strings.TrimPrefix(r.URL.Path, "/whep/endpoint/")

		peerID := uuid.New()
		s.log.Infof("WHEP Negotiation: peer=%s status=started offer=none answer=none", peerID)

		w.Header().Add("Content-Type", "application/sdp")
		// In the future this could be used to load balance users to more appropriate media servers.
		// Strike that, LB happens mainly in the original routing call
		// Location header here is used to transfer state for the PC
		w.Header().Add("Location", "http://localhost:8091/whep/resource/"+peerID.String())
		w.WriteHeader(http.StatusCreated)

		peerConnection, err := webrtc.NewPeerConnection(webrtc.Configuration{
			ICEServers: []webrtc.ICEServer{
				{
					// URLs: []string{"stun:stun.l.google.com:19302"},
				},
			},
		})
		if err != nil {
			panic(err)
		}

		peerConnection.OnICEConnectionStateChange(func(connectionState webrtc.ICEConnectionState) {
			s.log.Debugf("Connection State has changed %s \n", connectionState.String())
		})

		// Importantly, the track needs to be added before the offer (duh!)
		tracks, err := s.control.GetTracks(1234)
		if err != nil {
			panic(err)
		}
		for _, track := range tracks {
			peerConnection.AddTrack(track)
		}

		peerConnections[peerID.String()] = peerConnection

		// Used for SDP offer generated by the WHEP endpoint
		offer, err := peerConnection.CreateOffer(nil)
		if err != nil {
			panic(err)
		}
		gatherComplete := webrtc.GatheringCompletePromise(peerConnection)
		if err = peerConnection.SetLocalDescription(offer); err != nil {
			panic(err)
		}
		<-gatherComplete

		// If you don't do this, everything sucks...
		localDescription := peerConnection.LocalDescription()

		s.log.Infof("WHEP Negotiation: peer=%s status=negotiating offer=created answer=none", peerID)

		// fmt.Printf("Raw Offer from Pion: %#v", offer.SDP)

		fmt.Fprint(w, string(localDescription.SDP))
	})

	// Player (Nothing) => Endpoint (Offer) => Player (Answer)
	// This function actually finishes the SDP handshake
	// After this the WebRTC connection should be established
	http.HandleFunc("/whep/resource/", func(w http.ResponseWriter, r *http.Request) {
		unsafePcID := strings.TrimPrefix(r.URL.Path, "/whep/resource/")
		body, err := ioutil.ReadAll(r.Body)
		if err != nil {
			panic(err)
		}
		// Check for lookupPc in peerConnections
		s.log.Infof("WHEP Negotiation: peer=%s status=negotiating offer=accepted answer=created", unsafePcID)

		answer := webrtc.SessionDescription{Type: webrtc.SDPTypeAnswer, SDP: string(body)}

		if err = peerConnections[unsafePcID].SetRemoteDescription(answer); err != nil {
			panic(err)
		}

		s.log.Infof("WHEP Negotiation: peer=%s status=negotiated offer=accepted answer=accepted", unsafePcID)

		w.Header().Add("Content-Type", "application/sdp")
		// w.Header().Add("Location", "http://localhost:8091/whep/resource/1234")

		w.WriteHeader(http.StatusNoContent)

		fmt.Fprintf(w, "")
	})

	http.HandleFunc("/stream/", func(w http.ResponseWriter, r *http.Request) {
		streamID := strings.TrimPrefix(r.URL.Path, "/stream/")
		data := struct {
			StreamID string
		}{StreamID: streamID}

		streamTemplate.Execute(w, data)
	})

	s.log.Fatal(http.ListenAndServe(s.config.Address, nil))
}
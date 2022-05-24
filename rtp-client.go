// go run sender.go --url="wss://$(minikube ip):8447/one2one"

package main

import (
	"crypto/tls"
	"net"
	"flag"
	"fmt"
	"log"
	"errors"
	"os"
	"path"
	"sync"
	"context"
	"strings"
	// "os/signal"
	// "net/http"
	"encoding/json"

	"github.com/gorilla/websocket"
	"github.com/pion/webrtc/v3"
	"github.com/pion/ice/v2"

	"webrtc-client-go/wmsg"
	"webrtc-client-go/wcodec"
)

var Usage = func() {
	fmt.Fprintf(os.Stderr, "%s <caller|callee> [args]\n", path.Base(os.Args[0]))
	flag.PrintDefaults()
	os.Exit(1)
}

const (
	FREE int = iota
	SETUP
	BUSY
)

const DefaultUrl = "ws://localhost:8443/"

func describe(i interface{}) {
	fmt.Printf("(%v, %T)\n", i, i)
}

// must be called when holding the candidateLock
func processCachedCandidates(pc *webrtc.PeerConnection, cache []webrtc.ICECandidateInit) () {
	for len(cache) > 0 {
		var c webrtc.ICECandidateInit
		c, cache = cache[0], cache[1:]
		log.Println("Adding cached remote ICE candidate:", c,
			"SdpMid:", *c.SDPMid, "SdpMLineIndex:", *c.SDPMLineIndex)
		if candidateErr := pc.AddICECandidate(c); candidateErr != nil {
			log.Fatal(candidateErr)
		}
	}
}

func getIfaceForAddr(addr string) (string, error) {
	if(addr == "") {
		return "", errors.New("no IP given")
	}
	ifaces, err := net.Interfaces()
	if err != nil {
		log.Println("could not obtain local interface list:", err)
		return "", errors.New("net.Interfaces")
	}
	for _, i := range ifaces {
		addrs, err := i.Addrs()
		if err != nil {
			log.Printf("could not obtain IP address for interface %s: %v",
				i.Name, err)
			return "", errors.New("net.Addrs")
		}
		for _, a := range addrs {
			ipnet, ok := a.(*net.IPNet)
			if !ok { continue }
			v4 := ipnet.IP.To4()
			if v4 == nil { continue }
			if(v4.String() == addr){
				// log.Println("considering ifs:", a.String(), ", ", addr)
				return i.Name, nil
			}
		}
	}
	return "", errors.New("addr not found")
}

// rewrites protocol string to UDP/
func rewriteProto(desc *webrtc.SessionDescription) (*webrtc.SessionDescription) {
	parsed, err := desc.Unmarshal()
	if err != nil {
		log.Fatal("cannot parse SDP:", desc)
	}

	if len(parsed.MediaDescriptions) > 0 {
		parsed.MediaDescriptions[0].MediaName.Protos = []string{"RTP", "AVP"}
	}

	sdp, err := parsed.Marshal()
	if err != nil {
		log.Fatal("cannot serialize SDP:", parsed)
	}

	return &webrtc.SessionDescription{
		SDP:    string(sdp),
		Type:   desc.Type,
	}
}

func main() {
	state := FREE
	log.SetFlags(log.LstdFlags | log.Lshortfile)

	// done := make(chan os.Signal, 1)
	// signal.Notify(done, os.Interrupt)

	// we need to consume the first positional arg
	role := ""
	if len(os.Args) > 1 && (os.Args[1] == "caller" || os.Args[1] == "callee") {
		role = os.Args[1]
		os.Args = os.Args[1:]
	} else {
		Usage()
	}

	// cmd line
	Url      := flag.String("url", DefaultUrl, "WebRtc server URL")
	TLSDebug := flag.Bool("debug", false, "Debug the TLS connection using a keylogger: dumps data into /tmp/keylog")
	// audioSrc := flag.String("audio", "audiotestsrc", "GStreamer audio src")
	file     := flag.String("file", "", "caller: media file to send / callee: media file to write (extension is either h264 or vp8/ivf, this selects receiver side codec)")
	user     := flag.String("user", "test1", "User name (will be registered with the WebRTC server)")
	peer     := flag.String("peer", "test2", "Peer name (will be registered with the WebRTC server)")
	iceAddr  := flag.String("ice-addr", "", "Use only the given IP address to generate local ICE candidates")
	flag.Parse();

	// Assert that we have an audio or video file
	_, err := os.Stat(*file)
	if role == "caller" && os.IsNotExist(err) {
		log.Fatalf("Could not open file `%s`: %s\n", file, err)
	}

	// Select the receiver side codec
	codec := webrtc.MimeTypeH264
	switch strings.ToLower(path.Ext(*file)){
	case ".h264", ".mkv":
		codec = webrtc.MimeTypeH264
	case ".vp8", ".ivf":
		codec = webrtc.MimeTypeVP8
	default:
		log.Fatalf("Unknown codec %s: file extension must be either mkv/h264 or vp8/ivf",
			strings.ToLower(path.Ext(*file)))
	}
	
	log.Printf("Starting %s: user=%s, peer=%s: video: %s\n", role, *user, *peer, *file)

	//server uses self-signed certificate: switch to insecure TLS mode
	dialer := *websocket.DefaultDialer
	dialer.TLSClientConfig = &tls.Config{InsecureSkipVerify: true}
	if *TLSDebug {
		kl, err := os.OpenFile("/tmp/keylog", os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0600)
		if err != nil {
			log.Fatal("keylog:", err)
		}
		defer kl.Close()
		dialer.TLSClientConfig.KeyLogWriter = kl
		fmt.Fprintf(kl, "# SSL/TLS secrets log file, generated by go\n")
	}

	// connect to the webrtc-server
	log.Printf("connecting to %s", *Url)
	c, _, err := dialer.Dial(*Url, nil)
	if err != nil {
		log.Fatalln("dial:", err)
	}
	defer c.Close()

	// setup a peerconnection so that we can generate an SDP
	s := webrtc.SettingEngine{}

	// we are not interested in TPC or anything IPv6 for simplicity
	s.SetNetworkTypes([]webrtc.NetworkType{webrtc.NetworkTypeUDP4})

	// disable Multicast DNS
	s.SetICEMulticastDNSMode(ice.MulticastDNSModeDisabled)
	
	// filter ice candidates: if iceAddr is available from the command line, we generate ICE
	// candidates only on the interface that has this IP: this removes lots of useless ICE
	// trials
	iceIface, err := getIfaceForAddr(*iceAddr)
	if(err == nil){
		log.Println("Using ICE interface:", iceIface)
		s.SetInterfaceFilter(func(i string) bool { return i == iceIface } )
	} else {
		log.Println("Failed to use ICE interface:", err)
	}

	// s.SetAnsweringDTLSRole(webrtc.DTLSRoleClient)
	s.SetAnsweringDTLSRole(webrtc.DTLSRoleServer)
	
	// set up media codecs to enforce transcoding (use m.RegisterDefaultCodecs() if transcoding
	// is not needed
	m := &webrtc.MediaEngine{}

	var regCodecs []webrtc.RTPCodecParameters
	switch codec {
	case webrtc.MimeTypeVP8:
		regCodecs = wcodec.VP8Codecs
	case webrtc.MimeTypeH264:	
		regCodecs = wcodec.H264Codecs
	}

	for _, c := range regCodecs {
		if err := m.RegisterCodec(c, webrtc.RTPCodecTypeVideo); err != nil {
			log.Fatalln("Could not register codec:", c)
		}
	}

	// we don't want to use public ICE/STUN servers: we _know_ and control the IPs in our tests
	config := webrtc.Configuration{
		ICEServers: []webrtc.ICEServer{
			{
				URLs: []string{},
			},
		},
	}

	// setup the peer-connection at last
	peerConnection, err :=
		webrtc.NewAPI(webrtc.WithSettingEngine(s),webrtc.WithMediaEngine(m)).NewPeerConnection(config)
	if err != nil {
		log.Fatalln("NewPeerConnection:", err)
	}

	peerConnection.OnSignalingStateChange(func(ss webrtc.SignalingState) {
		log.Println("Signaling state change:", ss)
	})

	// Set the handler for ICE connection state
	// This will notify you when the peer has connected/disconnected
	_, iceConnectedCtxCancel := context.WithCancel(context.Background())
	peerConnection.OnICEConnectionStateChange(func(connectionState webrtc.ICEConnectionState) {
		log.Println("Connection state change:", connectionState.String())
		switch connectionState {
		case webrtc.ICEConnectionStateConnected:
			iceConnectedCtxCancel()
		case webrtc.ICEConnectionStateDisconnected, webrtc.ICEConnectionStateFailed:
			log.Fatalln("Disconnected/failed, exiting")
		}
	})
	
	// reader: get messages from the webrtc-server
	candidateLock := sync.RWMutex{}
	candidateCache := []webrtc.ICECandidateInit{}
	recv := make(chan map[string]interface{})
	go func() {
		defer close(recv)
		for {
			_, message, err := c.ReadMessage()
			if err != nil {
				// if websocket.IsUnexpectedCloseError(err, websocket.CloseGoingAway) {
				// 	log.Fatalf("trying to read from closed websocket: %s\n", err)
				// }
				log.Println("readMessage:", err)
				// if websocket.IsUnexpectedCloseError(err, websocket.CloseGoingAway) {
					// return
				// }
				return
			}

			// dunno the types yet, try to unmarschal
			m := map[string]interface{}{}
			err = json.Unmarshal(message, &m)
			if err != nil {
				log.Println("JSON unmarschal:", err)
				continue
			}

			log.Printf("recv: %v", m)

			// handle REMOTE ICECandidates: cache or add
			if m["id"].(string) == "iceCandidate" {
				c := m["candidate"].(map[string]interface{})
				sdpmid := c["sdpMid"].(string)
				sdpmlineindex := uint16(c["sdpMLineIndex"].(float64))
				candidate := webrtc.ICECandidateInit{
					Candidate: c["candidate"].(string),
					SDPMid: &sdpmid,
					SDPMLineIndex: &sdpmlineindex,
				}
				if peerConnection.RemoteDescription() == nil {
					// no remote SDP yet, cache candidate
					log.Println("Caching remote ICE candidate:", candidate,
						"SdpMid:", *candidate.SDPMid,
						"SdpMLineIndex:", *candidate.SDPMLineIndex)
					candidateLock.Lock()
					candidateCache = append(candidateCache, candidate)
					candidateLock.Unlock()
				} else {
					log.Println("Adding remote ICE candidate:", candidate,
						"SdpMid:", *candidate.SDPMid,
						"SdpMLineIndex:", *candidate.SDPMLineIndex)
					if candidateErr := peerConnection.AddICECandidate(candidate); candidateErr != nil {
						log.Fatal(candidateErr)
					}
				}
				continue;
			}

			recv <- m
		}
	}()

	// sender: write messages to the rtp-server
	send := make(chan wmsg.Message)
	go func() {
		defer close(send)
		for s := range send {
			err := c.WriteJSON(s)
			if err != nil {
				log.Println("WriteJSON:", err)
				return
			}
			log.Printf("send: %s", s)
		}
	}()

	// register
	log.Println("registering user:", *user)
	r := wmsg.NewRegisterRequest(*user)
	send <- r

	reply, err := wmsg.NewRegisterResponse(<- recv)
	if err != nil {
		log.Fatal(err)
	}
	if reply.Response != "accepted" {
		log.Fatalf("could not register caller %s: %s\n", user, reply.Response)
	}

	// handle LOCAL ICE candidates
	// channel that is blocked until LOCAL!! ICE Gathering is complete
	gatherComplete := webrtc.GatheringCompletePromise(peerConnection)
	peerConnection.OnICECandidate(func(i *webrtc.ICECandidate) {
		if i != nil {
			// send <- wmsg.NewOnICECandidate(i)
			log.Println("Found new ICE candidate:", *i)
		}
	})

	switch role {
	case "caller":
		log.Printf("starting call: %s -> %s\n", *user, *peer)

		// audio&video
		videoTrack, videoTrackErr := webrtc.NewTrackLocalStaticSample(
			webrtc.RTPCodecCapability{MimeType: codec}, "video", "pion")
		if videoTrackErr != nil {
			log.Fatalln(videoTrackErr)
		}

		_, videoTrackErr = peerConnection.AddTrack(videoTrack)
		if videoTrackErr != nil {
			log.Fatalln(videoTrackErr)
		}

		offer, err := peerConnection.CreateOffer(nil)
		if err != nil {
			log.Fatalln("cannot create offer:", err)
		}

		if err = peerConnection.SetLocalDescription(offer); err != nil {
			log.Fatalln("cannot set local SDP:", err)
		}

		// we can't use the offer as is:
		// - no ICE, we use the first ICE candidate to set up the address
		// - no DTLS, rewrite protocol
		<-gatherComplete
		new_offer := peerConnection.LocalDescription()
		new_offer = rewriteProto(new_offer)
		// describe(*new_offer)
		
		state = SETUP
		send <- wmsg.NewCallRequest(*user, *peer, new_offer.SDP)

		// wait for a call response
		var call_res wmsg.CallResponse
		for {
			call_res, err = wmsg.NewCallResponse(<- recv)
			if err != nil {
				log.Println("NewCallResponse:", err)
				continue
			}
			break
		}
		log.Println("call response:", call_res.Response)

		if call_res.Response != "accepted" {
			log.Fatalln("call rejected with message:", call_res.Response)
		}
		state = BUSY
		log.Println("new state:", state)

		desc := webrtc.SessionDescription{Type: webrtc.SDPTypeAnswer, SDP: call_res.Sdp}
		log.Printf("Remote session description received: %v\n", desc)
		// err = peerConnection.SetRemoteDescription(desc)
		// if err != nil {
		// 	log.Fatalln("cannot set remote SDP:", err)
		// }

		// // process remaining cached REMOTE ICE candidates
		// candidateLock.Lock()
		// processCachedCandidates(peerConnection, candidateCache);
		// candidateLock.Unlock()

		log.Println("connection setup ready")
		peerConnection.Close()

		// Start pushing buffers on these tracks
		wcodec.RTPSendFile(new_offer, &desc, *file, codec, videoTrack)

		// Block forever
		select {}

	case "callee":

		// Allow us to receive 1 video track
		if _, err = peerConnection.AddTransceiverFromKind(webrtc.RTPCodecTypeVideo); err != nil {
			log.Fatalln(err)
		}

		// audio
		// if _, err = peerConnection.AddTransceiverFromKind(webrtc.RTPCodecTypeAudio); err != nil {
		// 	panic(err)
		// }

		// wait for a call request
		var inc_req wmsg.IncomingCallRequest
		for {
			inc_req, err = wmsg.NewIncomingCallRequest(<- recv)
			if err != nil {
				log.Println("NewIncomingCallRequest:", err)
				continue
			}
			break
		}
		log.Println("new call from:", inc_req.From)

		// create offer
		offer, err := peerConnection.CreateOffer(nil)
		if err != nil {
			log.Fatalln("cannot create offer:", err)
		}
		// describe(offer)

		// respond: accept
		state = SETUP
		if err = peerConnection.SetLocalDescription(offer); err != nil {
			log.Fatalln("cannot set local SDP:", err)
		}

		// we can't use the offer as is:
		// - no ICE, we use the first ICE candidate to set up the address
		// - no DTLS, rewrite protocol
		<-gatherComplete
		new_offer := peerConnection.LocalDescription()
		new_offer = rewriteProto(new_offer)
		// describe(*new_offer)
		
		send <- wmsg.NewIncomingCallResponse(*user, "accept", new_offer.SDP)

		// wait for a startCommunication message
		var start_com wmsg.StartCommunication
		for {
			start_com, err = wmsg.NewStartCommunication(<- recv)
			if err != nil {
				log.Println("NewStartCommunication:", err)
				continue
			}
			break
		}
		log.Println("start communication:")

		desc := webrtc.SessionDescription{Type: webrtc.SDPTypeAnswer, SDP: start_com.Sdp}
		log.Printf("Remote session description received: %v\n", desc)
		// err = peerConnection.SetRemoteDescription(desc)
		// if err != nil {
		// 	log.Fatalln("cannot set remote SDP:", err)
		// }

		state = BUSY
		log.Println("new state:", state)

		// // process remaining cached REMOTE ICE candidates
		// candidateLock.Lock()
		// processCachedCandidates(peerConnection, candidateCache);
		// candidateLock.Unlock()

		log.Println("connection setup ready")
		peerConnection.Close()

		// Set a handler for when a new remote track starts
		wcodec.RTPReceiveTrack(new_offer, &desc, codec, *file)
		
		// Block forever
		select {}

	default:
		log.Printf("Unknown role: %s\n", role)
		os.Exit(1)
	}

	os.Exit(0)

}

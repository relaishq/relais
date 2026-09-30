package server

import (
	"context"
	"encoding/json"
	"net/http"
	"sort"
	"time"

	"github.com/gorilla/websocket"
	pion "github.com/pion/webrtc/v3"
	"github.com/pion/webrtc/v3/pkg/media"
	"github.com/relais/pkg/logging"
	"github.com/relais/pkg/metrics"
	"github.com/relais/pkg/storage"
	"github.com/relais/pkg/webrtc"
	"github.com/sirupsen/logrus"
)

// SignalingServer handles WebRTC signaling
type SignalingServer struct {
	upgrader   websocket.Upgrader
	sessionMgr *SessionManager
	webrtcMgr  *webrtc.PionAdapter
	store      storage.Storage
	logger     *logging.Logger
	// clients    sync.Map // TODO: Track active WebSocket connections for broadcasting
}

// NewSignalingServer creates a new signaling server
func NewSignalingServer(sessionMgr *SessionManager, webrtcMgr *webrtc.PionAdapter) *SignalingServer {
	return &SignalingServer{
		upgrader: websocket.Upgrader{
			CheckOrigin: func(r *http.Request) bool {
				return true // In production, implement proper origin checks
			},
		},
		sessionMgr: sessionMgr,
		webrtcMgr:  webrtcMgr,
		store:      nil,
		logger:     logging.NewLogger("info"),
	}
}

// SetStorage sets the storage backend for ingress/egress operations.
func (s *SignalingServer) SetStorage(st storage.Storage) {
	s.store = st
}

// HandleWebSocket upgrades HTTP connection to WebSocket
func (s *SignalingServer) HandleWebSocket(w http.ResponseWriter, r *http.Request) {
	conn, err := s.upgrader.Upgrade(w, r, nil)
	if err != nil {
		http.Error(w, "Could not upgrade connection", http.StatusInternalServerError)
		return
	}
	defer conn.Close()

	s.handleConnection(conn)
}

type wsMsg struct {
	Type    string
	Payload json.RawMessage `json:"payload"`
}

type offerPayload struct {
	SessionID     string `json:"session_id"`
	ParticipantID string `json:"participant_id"`
	SDP           string `json:"sdp"`
}

type answerPayload struct {
	SDP string `json:"sdp"`
}

type tricklePayload struct {
	Candidate     string  `json:"candidate"`
	SDPMid        *string `json:"sdpMid"`
	SDPMLineIndex *uint16 `json:"sdpMLineIndex"`
}

type selectTracksPayload struct {
	VideoTrackID string `json:"videoTrackId"`
	AudioTrackID string `json:"audioTrackId"`
}

type trackEvent struct {
	ID        string `json:"id"`
	Kind      string `json:"kind"`
	Codec     string `json:"codec"`
	ClockRate uint32 `json:"clockRate"`
}

type tracksListResp struct {
	Tracks []string `json:"tracks"`
}

type connState struct {
	sessionID            string
	participantID        string
	pc                   *pion.PeerConnection
	localVideo           *pion.TrackLocalStaticSample
	localAudio           *pion.TrackLocalStaticSample
	egressNextIdx        int64
	selectedVideoTrackID string
	selectedAudioTrackID string
	tailerCancel         context.CancelFunc
}

func (s *SignalingServer) handleConnection(conn *websocket.Conn) {
	var state connState
	for {
		messageType, message, err := conn.ReadMessage()
		if err != nil {
			return
		}

		var msg wsMsg
		if err := json.Unmarshal(message, &msg); err != nil {
			continue
		}

		switch msg.Type {
		case "offer":
			var payload offerPayload
			if err := json.Unmarshal(msg.Payload, &payload); err != nil {
				continue
			}
			if state.pc == nil {
				pc, err := s.webrtcMgr.CreatePeerConnection()
				if err != nil {
					continue
				}
				state.pc = pc
				state.sessionID = payload.SessionID
				state.participantID = payload.ParticipantID

				pc.OnICECandidate(func(c *pion.ICECandidate) {
					if c == nil {
						return
					}
					out := wsMsg{Type: "trickle", Payload: mustJSON(tricklePayload{Candidate: c.ToJSON().Candidate, SDPMid: c.ToJSON().SDPMid, SDPMLineIndex: c.ToJSON().SDPMLineIndex})}
					b, _ := json.Marshal(out)
					_ = conn.WriteMessage(websocket.TextMessage, b)
				})

				// Cancel tailer when connection closes/fails
				pc.OnConnectionStateChange(func(s pion.PeerConnectionState) {
					if s == pion.PeerConnectionStateClosed || s == pion.PeerConnectionStateFailed {
						if state.tailerCancel != nil {
							state.tailerCancel()
						}
					}
				})

				// Ingress: handle incoming tracks and persist frames
				pc.OnTrack(func(tr *pion.TrackRemote, _ *pion.RTPReceiver) {
					go func() {
						var idx int64
						// Emit a track event to the client when first packet arrives
						sentTrackEvent := false
						for {
							pkt, _, err := tr.ReadRTP()
							if err != nil {
								return
							}
							mediaType := "video"
							if tr.Kind() == pion.RTPCodecTypeAudio {
								mediaType = "audio"
							}
							// Do not auto-select tracks; rely on explicit select_tracks from client
							if !sentTrackEvent {
								ev := wsMsg{Type: "track", Payload: mustJSON(trackEvent{ID: tr.ID(), Kind: mediaType, Codec: tr.Codec().MimeType, ClockRate: tr.Codec().ClockRate})}
								if b, err := json.Marshal(ev); err == nil {
									_ = conn.WriteMessage(websocket.TextMessage, b)
								}
								sentTrackEvent = true
							}
							pktTS := pkt.Timestamp
							frame := storage.Frame{
								SessionID:  state.sessionID,
								Index:      idx,
								Data:       pkt.Payload,
								Timestamp:  time.Now(),
								IngestTime: time.Now(),
								MediaType:  mediaType,
								Codec:      tr.Codec().MimeType,
								KeyFrame:   false,
								TrackID:    tr.ID(),
								SSRC:       uint32(tr.SSRC()),
								ClockRate:  tr.Codec().ClockRate,
								RTPTime:    pktTS,
							}
							if s.store != nil {
								_ = s.store.PutFrame(context.Background(), frame)
							}
							idx++
						}
					}()
				})

				// Egress: prepare local tracks and spawn tailer
				// Create transceivers to ensure SDP advertises sendrecv
				_, _ = pc.AddTransceiverFromKind(pion.RTPCodecTypeVideo)
				_, _ = pc.AddTransceiverFromKind(pion.RTPCodecTypeAudio)

				// Create local tracks (defaults; TODO: align codecs dynamically)
				vTrack, vErr := pion.NewTrackLocalStaticSample(pion.RTPCodecCapability{MimeType: pion.MimeTypeH264}, "relais-video", "relais")
				if vErr == nil {
					if _, err := pc.AddTrack(vTrack); err == nil {
						state.localVideo = vTrack
					}
				}
				aTrack, aErr := pion.NewTrackLocalStaticSample(pion.RTPCodecCapability{MimeType: pion.MimeTypeOpus}, "relais-audio", "relais")
				if aErr == nil {
					if _, err := pc.AddTrack(aTrack); err == nil {
						state.localAudio = aTrack
					}
				}
			}
			if err := state.pc.SetRemoteDescription(pion.SessionDescription{Type: pion.SDPTypeOffer, SDP: payload.SDP}); err != nil {
				continue
			}
			answer, err := state.pc.CreateAnswer(nil)
			if err != nil {
				continue
			}
			if err := state.pc.SetLocalDescription(answer); err != nil {
				continue
			}

			// Start egress tailer after local description set
			if state.tailerCancel != nil {
				state.tailerCancel()
			}
			ctx, cancel := context.WithCancel(context.Background())
			state.tailerCancel = cancel
			go s.startEgressTailer(ctx, &state)

			resp := wsMsg{Type: "answer", Payload: mustJSON(answerPayload{SDP: answer.SDP})}
			b, _ := json.Marshal(resp)
			if err := conn.WriteMessage(messageType, b); err != nil {
				return
			}

		case "list_tracks":
			if s.store == nil {
				// reply with empty list
				msg := wsMsg{Type: "tracks", Payload: mustJSON(tracksListResp{Tracks: []string{}})}
				if b, err := json.Marshal(msg); err == nil {
					_ = conn.WriteMessage(websocket.TextMessage, b)
				}
				continue
			}
			tracks, err := s.store.ListTracks(context.Background(), state.sessionID)
			if err != nil {
				tracks = []string{}
			}
			msg := wsMsg{Type: "tracks", Payload: mustJSON(tracksListResp{Tracks: tracks})}
			if b, err := json.Marshal(msg); err == nil {
				_ = conn.WriteMessage(websocket.TextMessage, b)
			}

		case "trickle":
			var payload tricklePayload
			if err := json.Unmarshal(msg.Payload, &payload); err != nil {
				continue
			}
			if state.pc == nil {
				continue
			}
			ice := pion.ICECandidateInit{Candidate: payload.Candidate, SDPMid: payload.SDPMid, SDPMLineIndex: payload.SDPMLineIndex}
			if err := state.pc.AddICECandidate(ice); err != nil {
				// log error
			}

		case "select_tracks":
			var payload selectTracksPayload
			if err := json.Unmarshal(msg.Payload, &payload); err != nil {
				continue
			}
			if payload.VideoTrackID != "" {
				state.selectedVideoTrackID = payload.VideoTrackID
			}
			if payload.AudioTrackID != "" {
				state.selectedAudioTrackID = payload.AudioTrackID
			}

		default:
			// Unknown message type; ignore or log
		}
	}
}

func (s *SignalingServer) handleSignalingMessage(msg struct {
	Type    string          `json:"type"`
	Payload json.RawMessage `json:"payload"`
},
) []byte {
	// Deprecated path: logic handled directly in handleConnection
	return nil
}

func mustJSON(v any) json.RawMessage {
	b, _ := json.Marshal(v)
	return b
}

func (s *SignalingServer) startEgressTailer(ctx context.Context, state *connState) {
	if s.store == nil {
		return
	}
	// Telemetry counters (lightweight)
	var reads, emptyReads, framesOut, errCount int64
	lastSummary := time.Now()
	// If Redis storage with Streams enabled, prefer blocking stream read
	if rs, ok := s.store.(*storage.RedisStorage); ok && rs.StreamsEnabled() {
		groupMode := rs.StreamsGroupEnabled()
		sessLastID := "$"
		vidLastID := "$"
		audLastID := "$"
		// Attempt to resume from stored cursors
		if !groupMode {
			if id, err := rs.GetStreamCursor(ctx, state.sessionID, state.participantID, "session"); err == nil && id != "" {
				sessLastID = id
				s.logger.WithFields(logrus.Fields{"session": state.sessionID, "participant": state.participantID, "last_id": sessLastID}).Info("egress: resumed session stream")
			}
		}
		// Simple exponential backoff for stream read errors
		backoff := 50 * time.Millisecond
		maxBackoff := 1 * time.Second
		nextLog := time.Time{}
		for {
			select {
			case <-ctx.Done():
				return
			default:
			}
			if state.pc == nil || state.pc.ConnectionState() == pion.PeerConnectionStateClosed {
				return
			}
			// Determine read mode: per-track if selection is present, else session stream
			var frames []storage.Frame
			var nextID string
			var err error
			if state.selectedVideoTrackID == "" && state.selectedAudioTrackID == "" {
				t0 := time.Now()
				var ids []string
				if groupMode {
					var gf []storage.Frame
					gf, ids, err = rs.ReadStreamGroup(ctx, state.sessionID, state.participantID, 128, 2*time.Second)
					frames = gf
					if len(ids) > 0 {
						nextID = ids[len(ids)-1]
					}
				} else {
					frames, nextID, err = rs.ReadStream(ctx, state.sessionID, sessLastID, 128, 2*time.Second)
				}
				if err != nil {
					if time.Now().After(nextLog) {
						s.logger.WithFields(logrus.Fields{"session": state.sessionID, "participant": state.participantID, "op": "xread_session"}).WithError(err).Warn("egress: XREAD session error")
						nextLog = time.Now().Add(2 * time.Second)
					}
					time.Sleep(backoff)
					if backoff < maxBackoff {
						backoff *= 2
					}
					errCount++
					continue
				}
				metrics.EgressReadDuration.Observe(time.Since(t0).Seconds())
				backoff = 50 * time.Millisecond
				if !groupMode && nextID != "" {
					sessLastID = nextID
					// persist session cursor best-effort
					_ = rs.SetStreamCursor(ctx, state.sessionID, state.participantID, "session", sessLastID)
				}
				reads++
				metrics.EgressReads.WithLabelValues("session").Inc()
				// Ack after processing in group mode
				if groupMode && len(ids) > 0 {
					_ = rs.AckStream(ctx, state.sessionID, ids)
				}
			} else {
				// Try read video track (short block) then audio track
				if state.selectedVideoTrackID != "" {
					// Resume video cursor if first time on this selection
					if !groupMode {
						if vidLastID == "$" {
							if id, err := rs.GetStreamCursor(ctx, state.sessionID, state.participantID, "video:"+state.selectedVideoTrackID); err == nil && id != "" {
								vidLastID = id
								s.logger.WithFields(logrus.Fields{"session": state.sessionID, "participant": state.participantID, "track": state.selectedVideoTrackID, "last_id": vidLastID}).Info("egress: resumed video stream")
							}
						}
					}
					t0 := time.Now()
					var vf []storage.Frame
					var vNext string
					var vIDs []string
					var vErr error
					if groupMode {
						vf, vIDs, vErr = rs.ReadTrackStreamGroup(ctx, state.sessionID, state.selectedVideoTrackID, state.participantID, 128, 500*time.Millisecond)
						if len(vIDs) > 0 {
							vNext = vIDs[len(vIDs)-1]
						}
					} else {
						vf, vNext, vErr = rs.ReadTrackStream(ctx, state.sessionID, state.selectedVideoTrackID, vidLastID, 128, 500*time.Millisecond)
					}
					if vErr == nil && len(vf) > 0 {
						frames = append(frames, vf...)
						if !groupMode {
							vidLastID = vNext
							_ = rs.SetStreamCursor(ctx, state.sessionID, state.participantID, "video:"+state.selectedVideoTrackID, vidLastID)
						}
					}
					if vErr != nil {
						if time.Now().After(nextLog) {
							s.logger.WithFields(logrus.Fields{"session": state.sessionID, "participant": state.participantID, "track": state.selectedVideoTrackID, "op": "xread_video"}).WithError(vErr).Warn("egress: XREAD video error")
							nextLog = time.Now().Add(2 * time.Second)
						}
						time.Sleep(backoff)
						if backoff < maxBackoff {
							backoff *= 2
						}
						errCount++
					} else {
						backoff = 50 * time.Millisecond
					}
					metrics.EgressReads.WithLabelValues("track").Inc()
					metrics.EgressReadDuration.Observe(time.Since(t0).Seconds())
					if groupMode && len(vIDs) > 0 {
						_ = rs.AckTrackStream(ctx, state.sessionID, state.selectedVideoTrackID, vIDs)
					}
				}
				if state.selectedAudioTrackID != "" {
					if !groupMode {
						if audLastID == "$" {
							if id, err := rs.GetStreamCursor(ctx, state.sessionID, state.participantID, "audio:"+state.selectedAudioTrackID); err == nil && id != "" {
								audLastID = id
								s.logger.WithFields(logrus.Fields{"session": state.sessionID, "participant": state.participantID, "track": state.selectedAudioTrackID, "last_id": audLastID}).Info("egress: resumed audio stream")
							}
						}
					}
					t0 := time.Now()
					var af []storage.Frame
					var aNext string
					var aIDs []string
					var aErr error
					if groupMode {
						af, aIDs, aErr = rs.ReadTrackStreamGroup(ctx, state.sessionID, state.selectedAudioTrackID, state.participantID, 128, 500*time.Millisecond)
						if len(aIDs) > 0 {
							aNext = aIDs[len(aIDs)-1]
						}
					} else {
						af, aNext, aErr = rs.ReadTrackStream(ctx, state.sessionID, state.selectedAudioTrackID, audLastID, 128, 500*time.Millisecond)
					}
					if aErr == nil && len(af) > 0 {
						frames = append(frames, af...)
						if !groupMode {
							audLastID = aNext
							_ = rs.SetStreamCursor(ctx, state.sessionID, state.participantID, "audio:"+state.selectedAudioTrackID, audLastID)
						}
					}
					if aErr != nil {
						if time.Now().After(nextLog) {
							s.logger.WithFields(logrus.Fields{"session": state.sessionID, "participant": state.participantID, "track": state.selectedAudioTrackID, "op": "xread_audio"}).WithError(aErr).Warn("egress: XREAD audio error")
							nextLog = time.Now().Add(2 * time.Second)
						}
						time.Sleep(backoff)
						if backoff < maxBackoff {
							backoff *= 2
						}
						errCount++
					} else {
						backoff = 50 * time.Millisecond
					}
					metrics.EgressReads.WithLabelValues("track").Inc()
					metrics.EgressReadDuration.Observe(time.Since(t0).Seconds())
					if groupMode && len(aIDs) > 0 {
						_ = rs.AckTrackStream(ctx, state.sessionID, state.selectedAudioTrackID, aIDs)
					}
				}
				// Merge by ingest time to preserve cross-track ordering
				if len(frames) > 1 {
					sort.Slice(frames, func(i, j int) bool { return frames[i].IngestTime.Before(frames[j].IngestTime) })
				}
				// If no frames were read, small idle sleep to avoid tight loop
				if len(frames) == 0 {
					time.Sleep(50 * time.Millisecond)
					emptyReads++
					metrics.EgressEmptyPolls.Inc()
				}
				reads++
			}
			for _, f := range frames {
				// Route by media type
				switch f.MediaType {
				case "video":
					if state.localVideo != nil && state.selectedVideoTrackID != "" && f.TrackID == state.selectedVideoTrackID {
						_ = state.localVideo.WriteSample(media.Sample{Data: f.Data, Duration: 33 * time.Millisecond})
						framesOut++
						metrics.FramesForwarded.WithLabelValues("video").Inc()
					}
				case "audio":
					if state.localAudio != nil && state.selectedAudioTrackID != "" && f.TrackID == state.selectedAudioTrackID {
						_ = state.localAudio.WriteSample(media.Sample{Data: f.Data, Duration: 20 * time.Millisecond})
						framesOut++
						metrics.FramesForwarded.WithLabelValues("audio").Inc()
					}
				}
				if f.Index >= state.egressNextIdx {
					state.egressNextIdx = f.Index + 1
				}
			}
			// Periodic summary
			if time.Since(lastSummary) >= 5*time.Second {
				s.logger.WithFields(logrus.Fields{"session": state.sessionID, "reads": reads, "empty": emptyReads, "frames_out": framesOut, "errors": errCount}).Info("egress: stats")
				lastSummary = time.Now()
			}
		}
	} else {
		// Polling fallback with context-aware cancellation
		ticker := time.NewTicker(50 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
			if state.pc == nil || state.pc.ConnectionState() == pion.PeerConnectionStateClosed {
				return
			}
			frames, err := s.store.ListFramesSince(ctx, state.sessionID, state.egressNextIdx, 64)
			if err != nil || len(frames) == 0 {
				continue
			}
			for _, f := range frames {
				if f.Index < state.egressNextIdx {
					continue
				}
				switch f.MediaType {
				case "video":
					if state.localVideo != nil && state.selectedVideoTrackID != "" && f.TrackID == state.selectedVideoTrackID {
						_ = state.localVideo.WriteSample(media.Sample{Data: f.Data, Duration: 33 * time.Millisecond})
					}
				case "audio":
					if state.localAudio != nil && state.selectedAudioTrackID != "" && f.TrackID == state.selectedAudioTrackID {
						_ = state.localAudio.WriteSample(media.Sample{Data: f.Data, Duration: 20 * time.Millisecond})
					}
				}
				state.egressNextIdx = f.Index + 1
			}
		}
	}
}

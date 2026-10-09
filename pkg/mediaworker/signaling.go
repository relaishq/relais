package mediaworker

import (
	"errors"
	"io"
	"mime"
	"net/http"
)

const maxOfferSize = 64 << 10

// CallsPath is where the signaling handler accepts offers.
const CallsPath = "/calls"

// SignalingHandler returns the worker's WHIP-style signaling endpoint. A call
// starts with one HTTP exchange:
//
//	POST   /calls       body: SDP offer (application/sdp)
//	                    201 Created, body: SDP answer, Location: /calls/{id}
//	DELETE /calls/{id}  hangs up
func (w *Worker) SignalingHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST "+CallsPath, w.handleOffer)
	mux.HandleFunc("DELETE "+CallsPath+"/{id}", w.handleHangup)

	return mux
}

func (w *Worker) handleOffer(rw http.ResponseWriter, r *http.Request) {
	mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || mediaType != "application/sdp" {
		http.Error(rw, "Content-Type must be application/sdp", http.StatusUnsupportedMediaType)

		return
	}

	offer, err := io.ReadAll(http.MaxBytesReader(rw, r.Body, maxOfferSize))
	if err != nil {
		http.Error(rw, "cannot read offer: "+err.Error(), http.StatusBadRequest)

		return
	}

	id, answer, err := w.CreateSession(r.Context(), string(offer))
	switch {
	case errors.Is(err, ErrUnsupportedOffer):
		http.Error(rw, err.Error(), http.StatusBadRequest)

		return
	case errors.Is(err, ErrClosed):
		http.Error(rw, err.Error(), http.StatusServiceUnavailable)

		return
	case err != nil:
		w.log.Warnf("create session: %v", err)
		http.Error(rw, "cannot create session", http.StatusInternalServerError)

		return
	}

	rw.Header().Set("Content-Type", "application/sdp")
	rw.Header().Set("Location", CallsPath+"/"+id)
	rw.WriteHeader(http.StatusCreated)
	if _, err := io.WriteString(rw, answer); err != nil {
		w.log.Debugf("write answer for session %s: %v", id, err)
	}
}

func (w *Worker) handleHangup(rw http.ResponseWriter, r *http.Request) {
	if err := w.EndSession(r.PathValue("id")); err != nil {
		http.Error(rw, err.Error(), http.StatusNotFound)

		return
	}
	rw.WriteHeader(http.StatusOK)
}

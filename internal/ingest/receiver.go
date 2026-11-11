package ingest

import (
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"strings"
	"time"

	"github.com/pridkett/simple_agent_tracer/internal/semantic"
	"github.com/pridkett/simple_agent_tracer/internal/storage"
	collector "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	status "google.golang.org/genproto/googleapis/rpc/status"
	"google.golang.org/protobuf/proto"
)

const (
	compressedLimit    = 8 << 20
	decodedLimit       = 16 << 20
	batchLimit         = 2000
	retainedBatchLimit = 48 << 20
	mediaType          = "application/x-protobuf"
)

type Identity struct{ Project, Source string }

type Receiver struct {
	Store    *storage.Store
	Identify func(*http.Request) (Identity, bool, error)
	slots    chan struct{}
}

func New(store *storage.Store, identify func(*http.Request) (Identity, bool, error)) *Receiver {
	return &Receiver{store, identify, make(chan struct{}, 4)}
}

func (h *Receiver) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	identity, ok, err := h.Identify(r)
	if err != nil {
		retry(w, err)
		return
	}
	if !ok {
		w.Header().Set("WWW-Authenticate", "Bearer")
		fail(w, http.StatusUnauthorized, 16, "ingest credential required")
		return
	}
	media, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || media != mediaType {
		fail(w, http.StatusUnsupportedMediaType, 3, "use application/x-protobuf")
		return
	}
	if r.ContentLength > compressedLimit {
		fail(w, http.StatusRequestEntityTooLarge, 8, "request size limit exceeded")
		return
	}
	select {
	case h.slots <- struct{}{}:
		defer func() { <-h.slots }()
	default:
		w.Header().Set("Retry-After", "1")
		fail(w, http.StatusTooManyRequests, 8, "receiver busy")
		return
	}
	reader := io.Reader(http.MaxBytesReader(w, r.Body, compressedLimit))
	switch strings.ToLower(strings.TrimSpace(r.Header.Get("Content-Encoding"))) {
	case "", "identity":
	case "gzip":
		gz, err := gzip.NewReader(reader)
		if err != nil {
			readError(w, err)
			return
		}
		defer gz.Close()
		reader = gz
	default:
		fail(w, http.StatusUnsupportedMediaType, 3, "unsupported content encoding")
		return
	}
	body, err := io.ReadAll(io.LimitReader(reader, decodedLimit+1))
	if err != nil {
		readError(w, err)
		return
	}
	if len(body) > decodedLimit {
		fail(w, http.StatusRequestEntityTooLarge, 8, "decoded size limit exceeded")
		return
	}
	var request collector.ExportTraceServiceRequest
	if err = (proto.UnmarshalOptions{RecursionLimit: 64}).Unmarshal(body, &request); err != nil {
		fail(w, http.StatusBadRequest, 3, "invalid OTLP envelope")
		return
	}
	count := 0
	for _, resource := range request.ResourceSpans {
		for _, scope := range resource.ScopeSpans {
			count += len(scope.Spans)
			if count > batchLimit {
				fail(w, http.StatusRequestEntityTooLarge, 8, "span count limit exceeded")
				return
			}
		}
	}
	now := time.Now().UnixNano()
	var rejected int
	for attempt := 0; attempt < 2; attempt++ {
		project, err := h.Store.Project(r.Context(), identity.Project)
		if err != nil {
			retry(w, err)
			return
		}
		spans := make([]semantic.Span, 0, count)
		retained := 0
		for _, resource := range request.ResourceSpans {
			for _, scope := range resource.ScopeSpans {
				for _, raw := range scope.Spans {
					span, ok := semantic.Normalize(raw, resource.Resource, scope.Scope, [2]string{resource.SchemaUrl, scope.SchemaUrl}, identity.Source, now, project.Capture)
					if ok {
						retained += span.RetainedBytes
						if retained > retainedBatchLimit {
							fail(w, http.StatusRequestEntityTooLarge, 8, "normalized batch size limit exceeded")
							return
						}
						spans = append(spans, span)
					}
				}
			}
		}
		rejected = count - len(spans)
		err = h.Store.Ingest(r.Context(), project, identity.Source, spans)
		if errors.Is(err, storage.ErrCaptureChanged) && attempt == 0 {
			continue
		}
		if err != nil {
			retry(w, err)
			return
		}
		break
	}
	response := &collector.ExportTraceServiceResponse{}
	if rejected > 0 {
		response.PartialSuccess = &collector.ExportTracePartialSuccess{RejectedSpans: int64(rejected), ErrorMessage: "invalid span identifiers or timestamps"}
	}
	reply(w, http.StatusOK, response)
}

func readError(w http.ResponseWriter, err error) {
	var limit *http.MaxBytesError
	if errors.As(err, &limit) {
		fail(w, http.StatusRequestEntityTooLarge, 8, "request size limit exceeded")
		return
	}
	fail(w, http.StatusBadRequest, 3, "invalid request body")
}

func retry(w http.ResponseWriter, err error) {
	slog.Warn("OTLP storage unavailable", "errorType", fmt.Sprintf("%T", err))
	w.Header().Set("Retry-After", "1")
	fail(w, http.StatusServiceUnavailable, 14, "storage unavailable")
}

func fail(w http.ResponseWriter, code int, statusCode int32, message string) {
	reply(w, code, &status.Status{Code: statusCode, Message: message})
}

func reply(w http.ResponseWriter, code int, body proto.Message) {
	raw, err := proto.Marshal(body)
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", mediaType)
	w.WriteHeader(code)
	_, _ = w.Write(raw)
}

// Copyright The OpenTelemetry Authors
// SPDX-License-Identifier: Apache-2.0

package libhoneyreceiver // import "github.com/open-telemetry/opentelemetry-collector-contrib/receiver/libhoneyreceiver"

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net"
	"net/http"
	"runtime/debug"
	"strings"
	"sync"

	"github.com/vmihailenco/msgpack/v5"
	"go.opentelemetry.io/collector/component"
	"go.opentelemetry.io/collector/component/componentstatus"
	"go.opentelemetry.io/collector/consumer"
	"go.opentelemetry.io/collector/receiver"
	"go.opentelemetry.io/collector/receiver/receiverhelper"
	"go.uber.org/zap"

	"github.com/open-telemetry/opentelemetry-collector-contrib/internal/coreinternal/errorutil"
	"github.com/open-telemetry/opentelemetry-collector-contrib/receiver/libhoneyreceiver/encoder"
	"github.com/open-telemetry/opentelemetry-collector-contrib/receiver/libhoneyreceiver/internal/libhoneyevent"
	"github.com/open-telemetry/opentelemetry-collector-contrib/receiver/libhoneyreceiver/internal/parser"
	"github.com/open-telemetry/opentelemetry-collector-contrib/receiver/libhoneyreceiver/internal/response"
)

type libhoneyReceiver struct {
	cfg        *Config
	server     *http.Server
	nextTraces consumer.Traces
	nextLogs   consumer.Logs
	shutdownWG sync.WaitGroup
	obsreport  *receiverhelper.ObsReport
	settings   *receiver.Settings
}

func newLibhoneyReceiver(cfg *Config, set *receiver.Settings) (*libhoneyReceiver, error) {
	r := &libhoneyReceiver{
		cfg:        cfg,
		nextTraces: nil,
		settings:   set,
	}

	var err error
	r.obsreport, err = receiverhelper.NewObsReport(receiverhelper.ObsReportSettings{
		ReceiverID:             set.ID,
		Transport:              "http",
		ReceiverCreateSettings: *set,
	})
	if err != nil {
		return nil, err
	}

	return r, nil
}

func (r *libhoneyReceiver) startHTTPServer(ctx context.Context, host component.Host) error {
	// If HTTP is not enabled, nothing to start.
	if !r.cfg.HTTP.HasValue() {
		return nil
	}

	httpMux := http.NewServeMux()

	httpCfg := r.cfg.HTTP.Get()
	r.settings.Logger.Info("r.nextTraces is not null so httpTracesReceiver was added", zap.Int("paths", len(httpCfg.TracesURLPaths)))
	for _, path := range httpCfg.TracesURLPaths {
		httpMux.HandleFunc(path, r.withPanicRecovery(r.handleEvent))
		r.settings.Logger.Debug("Added path to HTTP server", zap.String("path", path))
	}

	if r.cfg.AuthAPI != "" {
		httpMux.HandleFunc("/1/auth", r.withPanicRecovery(r.handleAuth))
	}

	var err error
	if r.server, err = httpCfg.ToServer(ctx, host, r.settings.TelemetrySettings, httpMux); err != nil {
		return err
	}

	r.settings.Logger.Info("Starting HTTP server", zap.String("endpoint", httpCfg.Endpoint))
	var hln net.Listener
	if hln, err = httpCfg.ToListener(ctx); err != nil {
		return err
	}

	r.shutdownWG.Add(1)
	go func() {
		defer r.shutdownWG.Done()

		if err := r.server.Serve(hln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			componentstatus.ReportStatus(host, componentstatus.NewFatalErrorEvent(err))
		}
	}()
	return nil
}

func (r *libhoneyReceiver) Start(ctx context.Context, host component.Host) error {
	if err := r.startHTTPServer(ctx, host); err != nil {
		return errors.Join(err, r.Shutdown(ctx))
	}

	return nil
}

// Shutdown is a method to turn off receiving.
func (r *libhoneyReceiver) Shutdown(ctx context.Context) error {
	var err error

	if r.server != nil {
		err = r.server.Shutdown(ctx)
	}

	r.shutdownWG.Wait()
	return err
}

func (r *libhoneyReceiver) registerTraceConsumer(tc consumer.Traces) {
	r.nextTraces = tc
}

func (r *libhoneyReceiver) registerLogConsumer(tc consumer.Logs) {
	r.nextLogs = tc
}

// getSourceIP extracts the source IP address from the request
// It checks X-Forwarded-For and X-Real-IP headers first, then falls back to RemoteAddr
func getSourceIP(req *http.Request) string {
	// Safety check - return unknown if request is nil
	if req == nil {
		return "unknown"
	}

	// Check X-Forwarded-For header (can contain multiple IPs)
	if xff := req.Header.Get("X-Forwarded-For"); xff != "" {
		// Take the first IP if there are multiple
		if idx := strings.Index(xff, ","); idx != -1 {
			return strings.TrimSpace(xff[:idx])
		}
		return strings.TrimSpace(xff)
	}

	// Check X-Real-IP header
	if xri := req.Header.Get("X-Real-IP"); xri != "" {
		return xri
	}

	// Fall back to RemoteAddr
	// RemoteAddr is in the format "IP:port", so we need to extract just the IP
	if host, _, err := net.SplitHostPort(req.RemoteAddr); err == nil {
		return host
	}

	// If splitting fails, return RemoteAddr as-is
	return req.RemoteAddr
}

// withPanicRecovery wraps an HTTP handler with panic recovery middleware
func (r *libhoneyReceiver) withPanicRecovery(handler http.HandlerFunc) http.HandlerFunc {
	return func(resp http.ResponseWriter, req *http.Request) {
		defer func() {
			if panicVal := recover(); panicVal != nil {
				// Build log fields safely - avoid potential nil dereference
				logFields := []zap.Field{
					zap.Any("panic", panicVal),
					zap.String("stack_trace", string(debug.Stack())),
				}

				// Only access req if it's not nil
				if req != nil {
					logFields = append(logFields,
						zap.String("source_ip", getSourceIP(req)),
						zap.String("user_agent", req.Header.Get("User-Agent")),
						zap.String("method", req.Method),
						zap.String("content-encoding", req.Header.Get("Content-Encoding")))

					if req.URL != nil {
						logFields = append(logFields,
							zap.String("endpoint", req.URL.Path),
							zap.String("full_url", req.URL.String()))
					}
				}

				// Log the panic with stack trace, source IP, and user agent
				r.settings.Logger.Error("Panic in request handler", logFields...)

				// Return a proper error response
				resp.Header().Set("Content-Type", "application/json")
				resp.WriteHeader(http.StatusBadRequest)

				// Write libhoney-compatible error response
				errorResponse := []response.ResponseInBatch{{
					ErrorStr: "failed to process request: internal error during decompression",
					Status:   http.StatusBadRequest,
				}}

				responseBody, _ := json.Marshal(errorResponse)
				_, _ = resp.Write(responseBody)
			}
		}()

		handler(resp, req)
	}
}

func (r *libhoneyReceiver) handleAuth(resp http.ResponseWriter, req *http.Request) {
	r.settings.Logger.Debug("handling auth request", zap.String("auth_api", r.cfg.AuthAPI))
	authURL := fmt.Sprintf("%s/1/auth", r.cfg.AuthAPI)
	authReq, err := http.NewRequest(http.MethodGet, authURL, http.NoBody)
	if err != nil {
		r.settings.Logger.Error("Failed to create auth request",
			zap.Error(err),
			zap.String("endpoint", "/1/auth"),
			zap.String("source_ip", getSourceIP(req)),
			zap.String("user_agent", req.Header.Get("User-Agent")))
		errJSON, _ := json.Marshal(`{"error": "failed to create auth request"}`)
		writeResponse(resp, "json", http.StatusBadRequest, errJSON)
		return
	}
	authReq.Header.Set("x-honeycomb-team", req.Header.Get("x-honeycomb-team"))
	var authClient http.Client
	authResp, err := authClient.Do(authReq)
	if err != nil {
		r.settings.Logger.Error("Failed to send auth request",
			zap.Error(err),
			zap.String("endpoint", "/1/auth"),
			zap.String("source_ip", getSourceIP(req)),
			zap.String("user_agent", req.Header.Get("User-Agent")))
		errJSON, _ := json.Marshal(fmt.Sprintf(`"error": "failed to send request to auth api endpoint", "message", %q}`, err.Error()))
		writeResponse(resp, "json", http.StatusBadRequest, errJSON)
		return
	}
	defer authResp.Body.Close()

	switch {
	case authResp.StatusCode == http.StatusUnauthorized:
		r.settings.Logger.Error("Auth request unauthorized",
			zap.String("endpoint", "/1/auth"),
			zap.String("source_ip", getSourceIP(req)),
			zap.String("user_agent", req.Header.Get("User-Agent")),
			zap.Int("status_code", authResp.StatusCode))
		errJSON, _ := json.Marshal(`"error": "received 401 response for authInfo request from Honeycomb API - check your API key"}`)
		writeResponse(resp, "json", http.StatusBadRequest, errJSON)
		return
	case authResp.StatusCode > 299:
		r.settings.Logger.Error("Bad response code from auth API",
			zap.String("endpoint", "/1/auth"),
			zap.String("source_ip", getSourceIP(req)),
			zap.String("user_agent", req.Header.Get("User-Agent")),
			zap.Int("status_code", authResp.StatusCode))
		errJSON, _ := json.Marshal(fmt.Sprintf(`"error": "bad response code from API", "status_code", %d}`, authResp.StatusCode))
		writeResponse(resp, "json", http.StatusBadRequest, errJSON)
		return
	}
	authRawBody, _ := io.ReadAll(authResp.Body)
	_, err = resp.Write(authRawBody)
	if err != nil {
		r.settings.Logger.Info("couldn't write http response")
	}
}

// writeLibhoneyError writes a bad request error response in the appropriate format for libhoney clients
func writeLibhoneyError(resp http.ResponseWriter, enc encoder.Encoder, errorMsg string) {
	errorResponse := []response.ResponseInBatch{{
		ErrorStr: errorMsg,
		Status:   http.StatusBadRequest,
	}}

	var responseBody []byte
	var err error
	var contentType string

	switch enc.ContentType() {
	case encoder.MsgpackContentType:
		responseBody, err = msgpack.Marshal(errorResponse)
		contentType = encoder.MsgpackContentType
	default:
		responseBody, err = json.Marshal(errorResponse)
		contentType = encoder.JSONContentType
	}

	if err != nil {
		// Fallback to generic error if we can't marshal the response
		errorutil.HTTPError(resp, err)
		return
	}
	writeResponse(resp, contentType, http.StatusBadRequest, responseBody)
}

func (r *libhoneyReceiver) handleEvent(resp http.ResponseWriter, req *http.Request) {
	// This handler is only called for event endpoints (/events, /event, /batch, etc)
	handlerEndpoint := "?event?" // Generic label for all event endpoints
	if req.URL != nil {
		handlerEndpoint = req.URL.Path
	}
	enc, ok := readContentType(resp, req)
	if !ok {
		return
	}

	dataset, err := parser.GetDatasetFromRequest(req.RequestURI)
	if err != nil {
		r.settings.Logger.Info("No dataset found in URL", zap.String("req.RequestURI", req.RequestURI))
	}

	// handleEvent is only called if an HTTP config is set, so HTTP must have a value.
	httpCfg := r.cfg.HTTP.Get()
	for _, p := range httpCfg.TracesURLPaths {
		dataset = strings.Replace(dataset, p, "", 1)
		r.settings.Logger.Debug("dataset parsed", zap.String("dataset.parsed", dataset))
	}
	var body []byte
	func() {
		defer func() {
			if panicVal := recover(); panicVal != nil {
				// Build log fields safely - avoid potential nil dereference
				logFields := []zap.Field{
					zap.Any("panic", panicVal),
					zap.String("stack_trace", string(debug.Stack())),
				}

				// Only access req if it's not nil
				if req != nil {
					logFields = append(logFields,
						zap.String("endpoint", handlerEndpoint),
						zap.String("source_ip", getSourceIP(req)),
						zap.String("user_agent", req.Header.Get("User-Agent")),
						zap.String("content-encoding", req.Header.Get("Content-Encoding")))
				}

				// Log the panic but don't expose internal details to the client
				r.settings.Logger.Error("Panic during request body read (likely malformed compressed data)", logFields...)
				err = errors.New("failed to read request body: invalid compressed data")
			}
		}()
		body, err = io.ReadAll(req.Body)
	}()

	if err != nil {
		r.settings.Logger.Error("Failed to read request body",
			zap.Error(err),
			zap.String("endpoint", handlerEndpoint),
			zap.String("source_ip", getSourceIP(req)),
			zap.String("user_agent", req.Header.Get("User-Agent")),
			zap.String("content-encoding", req.Header.Get("Content-Encoding")))
		writeLibhoneyError(resp, enc, "failed to read request body")
		// Don't try to drain body if we got an error from compressed reader
		// The reader may be in a corrupted state and cause another panic
		if req.Body != nil {
			func() {
				defer func() {
					if panicVal := recover(); panicVal != nil {
						logFields := []zap.Field{
							zap.Any("panic", panicVal),
							zap.String("endpoint", handlerEndpoint),
						}
						if req != nil {
							logFields = append(logFields,
								zap.String("source_ip", getSourceIP(req)),
								zap.String("user_agent", req.Header.Get("User-Agent")))
						}
						r.settings.Logger.Debug("Panic during body close after read error (expected with corrupted data)", logFields...)
					}
				}()
				_ = req.Body.Close()
			}()
		}
		return
	}
	func() {
		defer func() {
			if panicVal := recover(); panicVal != nil {
				logFields := []zap.Field{zap.Any("panic", panicVal)}
				if req != nil {
					logFields = append(logFields,
						zap.String("endpoint", handlerEndpoint),
						zap.String("source_ip", getSourceIP(req)),
						zap.String("user_agent", req.Header.Get("User-Agent")))
				}
				r.settings.Logger.Error("Panic during request body close", logFields...)
				writeLibhoneyError(resp, enc, "failed to close request body")
				err = errors.New("panic during body close")
			}
		}()
		err = req.Body.Close()
	}()
	if err != nil {
		if !strings.Contains(err.Error(), "panic during body close") {
			r.settings.Logger.Error("Failed to close request body",
				zap.Error(err),
				zap.String("endpoint", handlerEndpoint),
				zap.String("source_ip", getSourceIP(req)),
				zap.String("user_agent", req.Header.Get("User-Agent")))
			writeLibhoneyError(resp, enc, "failed to close request body")
		}
		return
	}
	libhoneyevents := make([]libhoneyevent.LibhoneyEvent, 0)
	switch req.Header.Get("Content-Type") {
	case "application/x-msgpack", "application/msgpack":
		// The custom UnmarshalMsgpack will handle timestamp normalization
		decoder := msgpack.NewDecoder(bytes.NewReader(body))
		decoder.UseLooseInterfaceDecoding(true)
		err = decoder.Decode(&libhoneyevents)
		if err != nil {
			r.settings.Logger.Info("messagepack decoding failed",
				zap.Error(err),
				zap.String("endpoint", handlerEndpoint),
				zap.String("source_ip", getSourceIP(req)),
				zap.String("user_agent", req.Header.Get("User-Agent")))
			writeLibhoneyError(resp, enc, "failed to unmarshal msgpack")
			return
		}
		if len(libhoneyevents) > 0 {
			// MsgPackTimestamp is guaranteed to be non-nil after UnmarshalMsgpack
			r.settings.Logger.Debug("Decoding with msgpack worked", zap.Time("timestamp.first.msgpacktimestamp", *libhoneyevents[0].MsgPackTimestamp), zap.String("timestamp.first.time", libhoneyevents[0].Time))
			r.settings.Logger.Debug("event zero", zap.String("event.data", libhoneyevents[0].DebugString()))
		}
	case encoder.JSONContentType:
		err = json.Unmarshal(body, &libhoneyevents)
		if err != nil {
			r.settings.Logger.Info("JSON decoding failed",
				zap.Error(err),
				zap.String("endpoint", handlerEndpoint),
				zap.String("source_ip", getSourceIP(req)),
				zap.String("user_agent", req.Header.Get("User-Agent")))
			writeLibhoneyError(resp, enc, "failed to unmarshal JSON")
			return
		}

		if len(libhoneyevents) > 0 {
			// Debug: Log the state of MsgPackTimestamp
			r.settings.Logger.Debug("JSON event decoded", zap.Bool("has_msgpacktimestamp", libhoneyevents[0].MsgPackTimestamp != nil))
			if libhoneyevents[0].MsgPackTimestamp != nil {
				r.settings.Logger.Debug("Decoding with json worked", zap.Time("timestamp.first.msgpacktimestamp", *libhoneyevents[0].MsgPackTimestamp), zap.String("timestamp.first.time", libhoneyevents[0].Time))
			} else {
				r.settings.Logger.Debug("Decoding with json worked", zap.String("timestamp.first.time", libhoneyevents[0].Time))
			}
		}
	default:
		r.settings.Logger.Info("unsupported content type",
			zap.String("content-type", req.Header.Get("Content-Type")),
			zap.String("endpoint", handlerEndpoint),
			zap.String("source_ip", getSourceIP(req)),
			zap.String("user_agent", req.Header.Get("User-Agent")))
	}

	// Parse events and track which original indices contributed to each OTLP entity
	otlpLogs, otlpTraces, indexMapping, parsingResults := parser.ToPdata(dataset, libhoneyevents, r.cfg.FieldMapConfig, *r.settings.Logger)

	// Use the request context which already contains client metadata when IncludeMetadata is enabled
	ctx := req.Context()

	// Start with parsing results, then apply batch processing results for span events/links
	results := parsingResults
	hasFailures := false

	// Check if any parsing failures occurred
	for _, result := range results {
		if result.Status != 0 && result.Status != http.StatusAccepted {
			hasFailures = true
		}
	}

	// Process logs - only override parsing results if consumer fails
	numLogs := otlpLogs.LogRecordCount()
	if numLogs > 0 {
		if r.nextLogs != nil {
			ctx = r.obsreport.StartLogsOp(ctx)
			err = r.nextLogs.ConsumeLogs(ctx, otlpLogs)
			r.obsreport.EndLogsOp(ctx, "protobuf", numLogs, err)
			// Only override parsing results if consumer failed
			if err != nil {
				applyConsumerResultsToSuccessfulEvents(results, indexMapping.LogIndices, err)
				hasFailures = true
			}
		} else {
			dropErr := errors.New("no log consumer configured")
			r.settings.Logger.Warn("Dropping log records - no log consumer configured", zap.Int("dropped_logs", numLogs))
			r.obsreport.EndLogsOp(ctx, "protobuf", numLogs, dropErr)
			// Override even successful parsing results since consumer is not configured
			applyConsumerResultsToSuccessfulEvents(results, indexMapping.LogIndices, dropErr)
			hasFailures = true
		}
	}

	// Process traces - only override parsing results if consumer fails
	numTraces := otlpTraces.SpanCount()
	if numTraces > 0 {
		if r.nextTraces != nil {
			ctx = r.obsreport.StartTracesOp(ctx)
			err = r.nextTraces.ConsumeTraces(ctx, otlpTraces)
			r.obsreport.EndTracesOp(ctx, "protobuf", numTraces, err)
			// Only override parsing results if consumer failed
			if err != nil {
				applyConsumerResultsToSuccessfulEvents(results, indexMapping.TraceIndices, err)
				hasFailures = true
			}
		} else {
			dropErr := errors.New("no trace consumer configured")
			r.settings.Logger.Warn("Dropping trace spans - no trace consumer configured", zap.Int("dropped_spans", numTraces))
			r.obsreport.EndTracesOp(ctx, "protobuf", numTraces, dropErr)
			// Override even successful parsing results since consumer is not configured
			applyConsumerResultsToSuccessfulEvents(results, indexMapping.TraceIndices, dropErr)
			hasFailures = true
		}
	}

	if err != nil {
		errorutil.HTTPError(resp, err)
		return
	}

	// Write response
	if hasFailures {
		writePartialResponse(resp, enc, results)
	} else {
		writeSuccessResponse(resp, enc, len(libhoneyevents))
	}
}

func readContentType(resp http.ResponseWriter, req *http.Request) (encoder.Encoder, bool) {
	if req.Method != http.MethodPost {
		handleUnmatchedMethod(resp)
		return nil, false
	}

	switch getMimeTypeFromContentType(req.Header.Get("Content-Type")) {
	case encoder.JSONContentType:
		return encoder.JsEncoder, true
	case "application/x-msgpack", "application/msgpack":
		return encoder.MpEncoder, true
	default:
		handleUnmatchedContentType(resp)
		return nil, false
	}
}

func writeResponse(w http.ResponseWriter, contentType string, statusCode int, msg []byte) {
	w.Header().Set("Content-Type", contentType)
	w.WriteHeader(statusCode)
	_, _ = w.Write(msg)
}

func getMimeTypeFromContentType(contentType string) string {
	mediatype, _, err := mime.ParseMediaType(contentType)
	if err != nil {
		return ""
	}
	return mediatype
}

func handleUnmatchedMethod(resp http.ResponseWriter) {
	status := http.StatusMethodNotAllowed
	writeResponse(resp, "text/plain", status, []byte(fmt.Sprintf("%v method not allowed, supported: [POST]", status)))
}

func handleUnmatchedContentType(resp http.ResponseWriter) {
	status := http.StatusUnsupportedMediaType
	writeResponse(resp, "text/plain", status, []byte(fmt.Sprintf("%v unsupported media type, supported: [%s, %s]", status, encoder.JSONContentType, encoder.PbContentType)))
}

// applyConsumerResultsToSuccessfulEvents applies consumer results only to events that succeeded parsing
func applyConsumerResultsToSuccessfulEvents(results []response.ResponseInBatch, indices []int, err error) {
	for _, idx := range indices {
		// Only override if the event was successfully parsed (status == 202)
		if results[idx].Status == http.StatusAccepted {
			if err != nil {
				results[idx] = response.ResponseInBatch{
					Status:   http.StatusServiceUnavailable,
					ErrorStr: err.Error(),
				}
			}
			// If consumer succeeded, keep the existing success status
		}
	}
}

// writeSuccessResponse writes a success response for all events in the batch
func writeSuccessResponse(resp http.ResponseWriter, enc encoder.Encoder, numEvents int) {
	successResponse := response.MakeSuccessResponse(numEvents)
	writeLibhoneyResponse(resp, enc, http.StatusOK, successResponse)
}

// writePartialResponse writes a response for mixed success/failure results
func writePartialResponse(resp http.ResponseWriter, enc encoder.Encoder, results []response.ResponseInBatch) {
	writeLibhoneyResponse(resp, enc, http.StatusOK, results)
}

// writeLibhoneyResponse marshals and writes a libhoney-format response
func writeLibhoneyResponse(resp http.ResponseWriter, enc encoder.Encoder, statusCode int, batchResponse []response.ResponseInBatch) {
	var responseBody []byte
	var err error
	var contentType string

	switch enc.ContentType() {
	case encoder.MsgpackContentType:
		responseBody, err = msgpack.Marshal(batchResponse)
		contentType = encoder.MsgpackContentType
	default:
		responseBody, err = json.Marshal(batchResponse)
		contentType = encoder.JSONContentType
	}

	if err != nil {
		// Fallback to generic error if we can't marshal the response
		errorutil.HTTPError(resp, err)
		return
	}
	writeResponse(resp, contentType, statusCode, responseBody)
}

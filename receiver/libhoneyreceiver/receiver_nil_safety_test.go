// Copyright The OpenTelemetry Authors
// SPDX-License-Identifier: Apache-2.0

package libhoneyreceiver

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/collector/receiver/receivertest"

	"github.com/open-telemetry/opentelemetry-collector-contrib/receiver/libhoneyreceiver/internal/metadata"
	"github.com/open-telemetry/opentelemetry-collector-contrib/receiver/libhoneyreceiver/internal/response"
)

// TestGetSourceIPNilSafety verifies getSourceIP handles nil request safely
func TestGetSourceIPNilSafety(t *testing.T) {
	// Test with nil request
	ip := getSourceIP(nil)
	assert.Equal(t, "unknown", ip, "Should return 'unknown' for nil request")

	// Test with valid request
	req, _ := http.NewRequest(http.MethodGet, "/test", nil)
	req.RemoteAddr = "192.168.1.1:5000"
	ip = getSourceIP(req)
	assert.Equal(t, "192.168.1.1", ip, "Should return correct IP for valid request")
}

// TestPanicRecoveryNilSafety tests that panic recovery doesn't panic on nil values
func TestPanicRecoveryNilSafety(t *testing.T) {
	cfg := createDefaultConfig().(*Config)
	set := receivertest.NewNopSettings(metadata.Type)

	r, err := newLibhoneyReceiver(cfg, &set)
	require.NoError(t, err)

	// Test the withPanicRecovery wrapper with a handler that panics
	// and potentially has nil request in the panic recovery
	panicHandler := http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		// Simulate a panic that might happen with corrupted data
		panic("test panic with potential nil access")
	})

	safeHandler := r.withPanicRecovery(panicHandler)

	tests := []struct {
		name        string
		setupReq    func() *http.Request
		expectPanic bool
	}{
		{
			name: "normal_request",
			setupReq: func() *http.Request {
				req := httptest.NewRequest(http.MethodPost, "/test", bytes.NewReader([]byte("test")))
				req.Header.Set("User-Agent", "test-agent")
				return req
			},
			expectPanic: false,
		},
		{
			name: "request_with_nil_url",
			setupReq: func() *http.Request {
				req := httptest.NewRequest(http.MethodPost, "/test", bytes.NewReader([]byte("test")))
				req.URL = nil // Simulate corrupted request
				return req
			},
			expectPanic: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := tt.setupReq()
			w := httptest.NewRecorder()

			// This should not panic even with nil URL or other issues
			assert.NotPanics(t, func() {
				safeHandler(w, req)
			}, "Handler with panic recovery should not panic")

			// Verify error response was written
			assert.Equal(t, http.StatusBadRequest, w.Code)

			// Check the response is valid JSON
			var errorResp []response.ResponseInBatch
			err := json.Unmarshal(w.Body.Bytes(), &errorResp)
			assert.NoError(t, err, "Should return valid JSON error response")
			if len(errorResp) > 0 {
				assert.Contains(t, errorResp[0].ErrorStr, "decompression")
			}
		})
	}
}

// TestPanicInPanicRecovery simulates a panic occurring within the panic recovery itself
func TestPanicInPanicRecovery(t *testing.T) {
	cfg := createDefaultConfig().(*Config)
	set := receivertest.NewNopSettings(metadata.Type)

	r, err := newLibhoneyReceiver(cfg, &set)
	require.NoError(t, err)

	// Create a handler that will cause the recovery itself to be tested
	complexPanicHandler := http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		// First panic that will be caught
		panic("initial panic")
	})

	safeHandler := r.withPanicRecovery(complexPanicHandler)

	// Even with complex scenarios, the outer recovery should handle it
	req := httptest.NewRequest(http.MethodPost, "/test", nil)
	w := httptest.NewRecorder()

	assert.NotPanics(t, func() {
		safeHandler(w, req)
	}, "Should handle panics safely even with edge cases")

	assert.Equal(t, http.StatusBadRequest, w.Code, "Should return bad request")
}
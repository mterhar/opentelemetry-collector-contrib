// Copyright The OpenTelemetry Authors
// SPDX-License-Identifier: Apache-2.0

package libhoneyreceiver

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestGetSourceIP(t *testing.T) {
	tests := []struct {
		name       string
		setupReq   func(*http.Request)
		expectedIP string
	}{
		{
			name: "RemoteAddr only",
			setupReq: func(req *http.Request) {
				req.RemoteAddr = "192.168.1.100:54321"
			},
			expectedIP: "192.168.1.100",
		},
		{
			name: "X-Forwarded-For single IP",
			setupReq: func(req *http.Request) {
				req.RemoteAddr = "10.0.0.1:54321"
				req.Header.Set("X-Forwarded-For", "203.0.113.195")
			},
			expectedIP: "203.0.113.195",
		},
		{
			name: "X-Forwarded-For multiple IPs",
			setupReq: func(req *http.Request) {
				req.RemoteAddr = "10.0.0.1:54321"
				req.Header.Set("X-Forwarded-For", "203.0.113.195, 70.41.3.18, 150.172.238.178")
			},
			expectedIP: "203.0.113.195",
		},
		{
			name: "X-Real-IP",
			setupReq: func(req *http.Request) {
				req.RemoteAddr = "10.0.0.1:54321"
				req.Header.Set("X-Real-IP", "198.51.100.178")
			},
			expectedIP: "198.51.100.178",
		},
		{
			name: "X-Forwarded-For takes precedence over X-Real-IP",
			setupReq: func(req *http.Request) {
				req.RemoteAddr = "10.0.0.1:54321"
				req.Header.Set("X-Forwarded-For", "203.0.113.195")
				req.Header.Set("X-Real-IP", "198.51.100.178")
			},
			expectedIP: "203.0.113.195",
		},
		{
			name: "X-Forwarded-For with spaces",
			setupReq: func(req *http.Request) {
				req.RemoteAddr = "10.0.0.1:54321"
				req.Header.Set("X-Forwarded-For", " 203.0.113.195 , 70.41.3.18 ")
			},
			expectedIP: "203.0.113.195",
		},
		{
			name: "IPv6 RemoteAddr",
			setupReq: func(req *http.Request) {
				req.RemoteAddr = "[2001:db8::1]:54321"
			},
			expectedIP: "2001:db8::1",
		},
		{
			name: "IPv6 X-Forwarded-For",
			setupReq: func(req *http.Request) {
				req.RemoteAddr = "[::1]:54321"
				req.Header.Set("X-Forwarded-For", "2001:db8::1, 2001:db8::2")
			},
			expectedIP: "2001:db8::1",
		},
		{
			name: "RemoteAddr without port",
			setupReq: func(req *http.Request) {
				req.RemoteAddr = "192.168.1.100"
			},
			expectedIP: "192.168.1.100",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req, _ := http.NewRequest(http.MethodGet, "/test", nil)
			tt.setupReq(req)

			actualIP := getSourceIP(req)
			assert.Equal(t, tt.expectedIP, actualIP, "Source IP mismatch")
		})
	}
}

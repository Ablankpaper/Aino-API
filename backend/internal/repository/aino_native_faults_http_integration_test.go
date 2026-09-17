//go:build integration && nativeconsumer

package repository_test

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
)

func (f *ainoNativeFaults) wrap(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if f.control(w, req) {
			return
		}
		path := req.URL.Path
		profile := req.Method == "GET" && (path == "/api/v1/auth/me" || path == "/api/v1/user/profile")
		refresh := req.Method == "POST" && path == "/api/v1/auth/refresh"
		logout := req.Method == "POST" && path == "/api/v1/auth/logout"
		credential := req.Method == "POST" && path == "/api/v1/desktop/credentials"
		inference := req.Method == "POST" && path == "/v1/chat/completions"
		f.mu.Lock()
		if profile {
			f.state.ProfileCalls++
		}
		offline := f.state.Offline && strings.HasPrefix(path, "/api/v1/")
		deny := profile && f.deniedAccess != "" && req.Header.Get("Authorization") == "Bearer "+f.deniedAccess
		f.mu.Unlock()
		if offline {
			conn, _, err := http.NewResponseController(w).Hijack()
			if err != nil {
				f.fail("native offline fault could not hijack real socket")
				http.Error(w, "socket drop failed", 500)
				return
			}
			f.count(func(s *ainoNativeFaultSnapshot) { s.NetworkDrops++ })
			_ = conn.Close()
			return
		}
		if deny {
			f.count(func(s *ainoNativeFaultSnapshot) { s.Profile401s++ })
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"code":401,"message":"Fixture rejected captured access token","reason":"TOKEN_EXPIRED"}`))
			return
		}
		if inference {
			f.count(func(s *ainoNativeFaultSnapshot) { s.InferenceRequests++ })
			// Reading before a held pre-auth request lets net/http observe a peer
			// disconnect; preserve the exact body for the real gateway afterward.
			if f.snapshot().HoldInferenceBeforeAuth {
				body, err := io.ReadAll(http.MaxBytesReader(w, req.Body, 32<<20))
				if err != nil {
					http.Error(w, "held inference body rejected", 400)
					return
				}
				req.Body = io.NopCloser(bytes.NewReader(body))
			}
			if !f.waitGate(req.Context(), "inference", &f.state.InferenceWaiting) {
				http.Error(w, "fixture inference gate ended", http.StatusGatewayTimeout)
				return
			}
			next.ServeHTTP(w, req)
			f.count(func(s *ainoNativeFaultSnapshot) { s.InferenceResponses++ })
			return
		}
		capture := path == "/api/v1/auth/phone/verify" || refresh || credential
		if !capture && !logout && !profile {
			next.ServeHTTP(w, req)
			return
		}
		f.mu.Lock()
		if refresh {
			f.state.RefreshStarted++
		}
		if logout {
			f.state.LogoutStarted++
			f.retireLease()
		}
		if credential {
			f.state.CredentialRequests++
		}
		f.mu.Unlock()
		rec := httptest.NewRecorder()
		next.ServeHTTP(rec, req)
		f.count(func(s *ainoNativeFaultSnapshot) {
			if refresh {
				s.RefreshCompleted++
			}
			if logout {
				s.LogoutCompleted++
			}
			if profile && rec.Code == http.StatusUnauthorized {
				s.Profile401s++
			}
			if credential && rec.Code == http.StatusOK {
				s.CredentialSuccesses++
			}
		})
		if capture && rec.Code == http.StatusOK {
			if err := f.captureIssued(rec.Body.Bytes()); err != nil {
				f.fail("could not capture real issued fixture credentials")
				http.Error(w, "credential capture failed", 500)
				return
			}
		}
		if refresh && !f.waitGate(req.Context(), "refresh", &f.state.RefreshWaiting) {
			http.Error(w, "fixture refresh gate ended", http.StatusGatewayTimeout)
			return
		}
		if logout && !f.waitGate(req.Context(), "logout", &f.state.LogoutWaiting) {
			http.Error(w, "fixture logout gate ended", http.StatusGatewayTimeout)
			return
		}
		if refresh && rec.Code == http.StatusOK {
			// Real access JWTs minted within one second can be byte-identical.
			// Once rotation's response is released, that legitimate token must
			// reach real middleware even if it equals the token originally armed.
			var envelope struct {
				Data struct {
					AccessToken string `json:"access_token"`
				} `json:"data"`
			}
			_ = json.Unmarshal(rec.Body.Bytes(), &envelope)
			f.mu.Lock()
			if f.deniedAccess == envelope.Data.AccessToken {
				f.deniedAccess = ""
			}
			f.mu.Unlock()
		}
		for name, values := range rec.Header() {
			for _, value := range values {
				w.Header().Add(name, value)
			}
		}
		w.WriteHeader(rec.Code)
		_, _ = w.Write(rec.Body.Bytes())
		f.count(func(s *ainoNativeFaultSnapshot) {
			if refresh {
				s.RefreshResponses++
			}
			if logout {
				s.LogoutResponses++
			}
		})
	})
}

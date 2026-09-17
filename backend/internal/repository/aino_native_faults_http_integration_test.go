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
		requestOwner := f.requestOwner(req)
		if refresh || logout {
			refreshOwner, err := f.refreshOwner(w, req)
			if err != nil {
				http.Error(w, "fixture auth body rejected", http.StatusBadRequest)
				return
			}
			if refreshOwner.valid() {
				requestOwner = refreshOwner
			}
		}
		requestUserID := requestOwner.userID
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
			f.mu.Lock()
			f.state.InferenceRequests++
			if user := f.users[requestUserID]; user != nil {
				user.inferenceRequests++
			}
			f.mu.Unlock()
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
			f.mu.Lock()
			f.state.InferenceResponses++
			if user := f.users[requestUserID]; user != nil {
				user.inferenceResponses++
			}
			f.mu.Unlock()
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
		}
		if credential {
			f.state.CredentialRequests++
			if user := f.users[requestUserID]; user != nil {
				user.credentialRequests++
			}
		}
		f.mu.Unlock()
		rec := httptest.NewRecorder()
		next.ServeHTTP(rec, req)
		if logout && rec.Code == http.StatusOK && requestOwner.valid() {
			f.mu.Lock()
			f.retireLease(requestOwner)
			f.mu.Unlock()
		}
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
		if credential && rec.Code == http.StatusOK {
			f.mu.Lock()
			if user := f.users[requestUserID]; user != nil {
				user.credentialSuccesses++
			}
			f.mu.Unlock()
		}
		if capture && rec.Code == http.StatusOK {
			if err := f.captureIssued(rec.Body.Bytes(), requestOwner); err != nil {
				f.fail("could not capture real issued fixture credentials")
				http.Error(w, "credential capture failed", 500)
				return
			}
		}
		f.mu.Lock()
		holdCredential := credential && f.state.HoldCredentialResponse && f.state.HoldCredentialUserID == requestUserID
		f.mu.Unlock()
		if holdCredential && !f.waitGate(req.Context(), f.credentialGateName(requestUserID), &f.state.CredentialWaiting) {
			http.Error(w, "fixture credential gate ended", http.StatusGatewayTimeout)
			return
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
		written, writeErr := w.Write(rec.Body.Bytes())
		f.count(func(s *ainoNativeFaultSnapshot) {
			if credential && rec.Code == http.StatusOK && writeErr == nil && written == rec.Body.Len() {
				s.CredentialResponses++
			}
			if refresh {
				s.RefreshResponses++
			}
			if logout {
				s.LogoutResponses++
			}
		})
	})
}

func (f *ainoNativeFaults) requestOwner(req *http.Request) ainoNativeCredentialOwner {
	const bearer = "Bearer "
	authorization := req.Header.Get("Authorization")
	if !strings.HasPrefix(authorization, bearer) {
		return ainoNativeCredentialOwner{}
	}
	credential := strings.TrimPrefix(authorization, bearer)
	f.mu.Lock()
	if owner := f.leaseOwners[credential]; owner.valid() {
		f.mu.Unlock()
		return owner
	}
	f.mu.Unlock()
	claims, err := f.rig.auth.ValidateToken(credential)
	if err != nil {
		return ainoNativeCredentialOwner{}
	}
	owner := ainoNativeCredentialOwner{userID: claims.UserID, family: claims.SessionID}
	f.mu.Lock()
	user := f.users[owner.userID]
	ok := false
	if user != nil {
		_, ok = user.sessions[owner.family]
	}
	f.mu.Unlock()
	if !ok {
		return ainoNativeCredentialOwner{}
	}
	return owner
}

func (f *ainoNativeFaults) refreshOwner(w http.ResponseWriter, req *http.Request) (ainoNativeCredentialOwner, error) {
	if req.Body == nil {
		return ainoNativeCredentialOwner{}, nil
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, req.Body, 4096))
	if err != nil {
		return ainoNativeCredentialOwner{}, err
	}
	req.Body = io.NopCloser(bytes.NewReader(body))
	var input struct {
		RefreshToken string `json:"refresh_token"`
	}
	if json.Unmarshal(body, &input) != nil || input.RefreshToken == "" {
		return ainoNativeCredentialOwner{}, nil
	}
	f.mu.Lock()
	owner := f.refreshOwners[input.RefreshToken]
	f.mu.Unlock()
	return owner, nil
}

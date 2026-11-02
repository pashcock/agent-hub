package mcpfacade

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestBearerTokenGatesTheMCPEndpoint(t *testing.T) {
	const token = "0123456789abcdef0123456789abcdef"
	reached := false
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reached = true
		w.WriteHeader(http.StatusOK)
	})
	guarded := RequireToken(token, inner)
	srv := httptest.NewServer(guarded)
	defer srv.Close()

	cases := []struct {
		name       string
		header     string
		wantStatus int
		wantReach  bool
	}{
		{"no header", "", http.StatusUnauthorized, false},
		{"wrong token", "Bearer nope", http.StatusUnauthorized, false},
		{"missing scheme", token, http.StatusUnauthorized, false},
		{"correct token", "Bearer " + token, http.StatusOK, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			reached = false
			req, _ := http.NewRequest(http.MethodGet, srv.URL, nil)
			if tc.header != "" {
				req.Header.Set("Authorization", tc.header)
			}
			res, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			defer res.Body.Close()
			if res.StatusCode != tc.wantStatus {
				t.Errorf("status = %d, want %d", res.StatusCode, tc.wantStatus)
			}
			if reached != tc.wantReach {
				t.Errorf("handler reached = %v, want %v", reached, tc.wantReach)
			}
		})
	}
}

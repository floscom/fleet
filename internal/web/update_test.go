package web

import (
	"context"
	"path/filepath"
	"testing"

	fleetv1 "fleet/gen/fleetv1"
)

func (s *fakeSource) Update(_ context.Context, req *fleetv1.UpdateRequest) (*fleetv1.UpdateStatus, error) {
	st := &fleetv1.UpdateStatus{Version: "v0.3.0", Auto: true, Latest: "v0.4.0"}
	if req.GetSetAuto() {
		st.Auto = req.GetAuto()
	}
	if req.GetInstall() {
		st.Installed = "v0.4.0"
	}
	return st, nil
}

func TestUpdateRoute(t *testing.T) {
	path := filepath.Join(t.TempDir(), "web-token")
	tok, _ := LoadOrCreateToken(path)
	ts := startServerToken(t, newFakeSource(), path)

	var e struct{ Error string }
	if code := api(t, ts, "GET", "/api/update", "wrong", "", &e); code != 401 {
		t.Fatalf("without admin: %d %+v", code, e)
	}
	var r struct{ Update UpdateStatus }
	if code := api(t, ts, "GET", "/api/update", tok, "", &r); code != 200 || r.Update != (UpdateStatus{Version: "v0.3.0", Auto: true, Latest: "v0.4.0"}) {
		t.Fatalf("get: %d %+v", code, r.Update)
	}
	if code := api(t, ts, "POST", "/api/update", tok, `{"auto":false}`, &r); code != 200 || r.Update.Auto {
		t.Fatalf("auto off: %d %+v", code, r.Update)
	}
	if code := api(t, ts, "POST", "/api/update", tok, `{"install":true}`, &r); code != 200 || r.Update.Installed != "v0.4.0" || !r.Update.Auto {
		t.Fatalf("install: %d %+v", code, r.Update)
	}
	if !forwardable("GET", "update") || !forwardable("POST", "update") {
		t.Fatal("updates of another fleet's daemon are not forwarded")
	}
}

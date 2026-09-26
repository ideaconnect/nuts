package nuts

import "testing"

func TestHandler_CaddyModule(t *testing.T) {
	h := Handler{}
	info := h.CaddyModule()

	if info.ID != "http.handlers.nuts" {
		t.Errorf("expected module ID %q, got %q", "http.handlers.nuts", info.ID)
	}

	module := info.New()
	if _, ok := module.(*Handler); !ok {
		t.Error("New() did not return *Handler")
	}
}

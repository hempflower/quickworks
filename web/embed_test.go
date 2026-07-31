package web

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestStaticServesCompiledTailwindCSS(t *testing.T) {
	response := httptest.NewRecorder()
	Static().ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/app.css", nil))
	if response.Code != http.StatusOK {
		t.Fatalf("unexpected stylesheet status: %d", response.Code)
	}
	if !strings.Contains(response.Body.String(), "tailwindcss") {
		t.Fatal("compiled Tailwind stylesheet was not embedded")
	}
}

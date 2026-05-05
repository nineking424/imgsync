package metrics

import (
	"net/http/httptest"
	"strings"
	"testing"
)

func TestNew_ExposesMetricsHandlerWithIsolatedRegistry(t *testing.T) {
	m := New()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/metrics", nil)

	m.Handler().ServeHTTP(rec, req)

	if rec.Code != 200 {
		t.Fatalf("want 200 got %d", rec.Code)
	}
	body := rec.Body.String()
	// 빈 registry 라도 promhttp 가 빈 본문 + 200 을 반환한다.
	// global default registry 와 달리 go_* / process_* 가 절대 안 보여야 한다.
	if strings.Contains(body, "go_goroutines") || strings.Contains(body, "process_cpu_seconds_total") {
		t.Fatalf("isolated registry leaked global metrics: %s", body)
	}
}

package main

import (
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

func TestMeshFailedCompletionDoesNotRefreshCell(t *testing.T) {
	for _, tc := range []struct {
		name, summary, terminal string
		want                    bool
	}{
		{"failed", `{"type":"ping","avg_ms":10,"loss_pct":0}`, `{"exit_ok":false}`, false},
		{"full-loss", `{"type":"ping","avg_ms":0,"loss_pct":100}`, `{"exit_ok":true}`, false},
		{"success", `{"type":"ping","avg_ms":10,"loss_pct":0}`, `{"exit_ok":true}`, true},
		{"legacy", `{"type":"ping","avg_ms":10,"loss_pct":0}`, "", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, _ := s11NewServer(t)
			probe := &meshProbe{fromID: "a", toID: "b", done: make(chan struct{}), summary: []byte(tc.summary)}
			srv.meshRuns["probe"] = probe
			srv.cmdNodes["probe"] = "a"
			srv.meshAcceptOutput(CommandResponse{ID: "probe", Type: "done", Data: tc.terminal})
			_, recorded := srv.snapshotMatrix()["a"]["b"]
			if recorded != tc.want || probe.succeeded() != tc.want {
				t.Fatalf("recorded=%v success=%v want=%v", recorded, probe.succeeded(), tc.want)
			}
			if len(srv.meshRuns) != 0 || len(srv.cmdNodes) != 0 {
				t.Fatal("completion leaked registrations")
			}
		})
	}
}

func TestScheduleOutputIncludesSeparatorInByteLimit(t *testing.T) {
	srv, _ := s11NewServer(t)
	sc := &Schedule{ID: "schedule", NodeID: "node", Enabled: true, Command: "ping", IntervalSec: 60}
	srv.schedules[sc.ID] = sc
	id, reason := srv.beginRun(sc)
	if reason != "" {
		t.Fatal(reason)
	}
	for _, data := range []string{strings.Repeat("x", scheduleMaxResult-1), "more"} {
		srv.scheduleAcceptOutput(CommandResponse{ID: id, Type: "output", Data: data})
	}
	if got := sc.currentBuf.Len(); got != scheduleMaxResult {
		t.Fatalf("buffer=%d want=%d", got, scheduleMaxResult)
	}
}

func TestUnreadableSchedulesArePreserved(t *testing.T) {
	for _, content := range []string{"[{broken", "{\"wrong\":true}"} {
		t.Run(content, func(t *testing.T) {
			srv, _ := s11NewServer(t)
			path := schedulesFilePath()
			if err := os.WriteFile(path, []byte(content), 0600); err != nil {
				t.Fatal(err)
			}
			srv.loadSchedules()
			srv.persistSchedules(path)
			got, err := os.ReadFile(path)
			if err != nil || string(got) != content {
				t.Fatalf("damaged file overwritten: %q %v", got, err)
			}
			for _, method := range []string{"GET", "POST"} {
				rec := httptest.NewRecorder()
				srv.handleSchedules(rec, httptest.NewRequest(method, "/api/schedules", strings.NewReader("{}")))
				if rec.Code != 503 {
					t.Fatalf("%s status=%d", method, rec.Code)
				}
			}
			rec := httptest.NewRecorder()
			srv.handleSchedule(rec, httptest.NewRequest("DELETE", "/api/schedules/example", nil))
			if rec.Code != 503 {
				t.Fatalf("DELETE status=%d", rec.Code)
			}
		})
	}
}

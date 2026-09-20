package atif

import (
	"strings"
	"testing"
)

func minimal(version string) *Trajectory {
	return &Trajectory{SchemaVersion: version, Agent: Agent{Name: "a", Version: "1"},
		Steps: []Step{{StepID: 1, Source: SourceUser, Message: Text("m")}}}
}

// TestValidateMatchesHarbor pins the checks where this validator had
// drifted from Harbor's models: closed media types with audio aliases
// normalised, a closed schema version list, and naive timestamps.
func TestValidateMatchesHarbor(t *testing.T) {
	tests := []struct {
		name string
		mod  func(*Trajectory)
		want string // substring of the error, "" for valid
	}{
		{"jpeg image", func(tr *Trajectory) {
			tr.Steps[0].Message = Content{Parts: []ContentPart{{Type: PartImage, Source: &MediaSource{MediaType: "image/jpeg", Path: "p"}}}}
		}, ""},
		{"svg image", func(tr *Trajectory) {
			tr.Steps[0].Message = Content{Parts: []ContentPart{{Type: PartImage, Source: &MediaSource{MediaType: "image/svg+xml", Path: "p"}}}}
		}, "image part with media_type"},
		{"heic image", func(tr *Trajectory) {
			tr.Steps[0].Message = Content{Parts: []ContentPart{{Type: PartImage, Source: &MediaSource{MediaType: "image/heic", Path: "p"}}}}
		}, "image part with media_type"},
		{"wav audio", func(tr *Trajectory) {
			tr.Steps[0].Message = Content{Parts: []ContentPart{{Type: PartAudio, Source: &MediaSource{MediaType: "audio/wav", Path: "p"}}}}
		}, ""},
		{"mp3 alias", func(tr *Trajectory) {
			tr.Steps[0].Message = Content{Parts: []ContentPart{{Type: PartAudio, Source: &MediaSource{MediaType: "audio/mp3", Path: "p"}}}}
		}, ""},
		{"x-m4a alias upper", func(tr *Trajectory) {
			tr.Steps[0].Message = Content{Parts: []ContentPart{{Type: PartAudio, Source: &MediaSource{MediaType: " Audio/X-M4A ", Path: "p"}}}}
		}, ""},
		{"opus audio", func(tr *Trajectory) {
			tr.Steps[0].Message = Content{Parts: []ContentPart{{Type: PartAudio, Source: &MediaSource{MediaType: "audio/opus", Path: "p"}}}}
		}, "audio part with media_type"},
		{"future minor", func(tr *Trajectory) { tr.SchemaVersion = "ATIF-v1.9" }, "not one Harbor accepts"},
		{"oldest", func(tr *Trajectory) { tr.SchemaVersion = "ATIF-v1.0" }, ""},
		{"offset timestamp", func(tr *Trajectory) { tr.Steps[0].Timestamp = "2026-09-20T14:30:00Z" }, ""},
		{"naive timestamp", func(tr *Trajectory) { tr.Steps[0].Timestamp = "2026-09-20T14:30:00" }, ""},
		{"naive with space", func(tr *Trajectory) { tr.Steps[0].Timestamp = "2026-09-20 14:30:00.5" }, ""},
		{"bare date", func(tr *Trajectory) { tr.Steps[0].Timestamp = "2026-09-20" }, ""},
		{"not a timestamp", func(tr *Trajectory) { tr.Steps[0].Timestamp = "yesterday" }, "not ISO 8601"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tr := minimal(SchemaVersion)
			tt.mod(tr)
			err := tr.Validate()
			switch {
			case tt.want == "" && err != nil:
				t.Errorf("Validate = %v, want valid", err)
			case tt.want != "" && (err == nil || !strings.Contains(err.Error(), tt.want)):
				t.Errorf("Validate = %v, want error containing %q", err, tt.want)
			}
		})
	}
}

// TestParseAcceptsFutureMinor: a reader must not refuse a document for
// a minor ATIF release it does not yet know; only Validate, which
// stands for Harbor's acceptance, enforces the closed list.
func TestParseAcceptsFutureMinor(t *testing.T) {
	if _, err := Parse([]byte(`{"schema_version":"ATIF-v1.12","agent":{"name":"a","version":"1"},"steps":[{"step_id":1,"source":"user","message":"m"}]}`)); err != nil {
		t.Errorf("Parse = %v", err)
	}
	if _, err := Parse([]byte(`{"schema_version":"ATIF-v2.0","agent":{"name":"a","version":"1"},"steps":[{"step_id":1,"source":"user","message":"m"}]}`)); err == nil {
		t.Error("Parse accepted a v2 document")
	}
}

func TestNormalizeAudioMediaType(t *testing.T) {
	for in, want := range map[string]string{"audio/mp3": "audio/mpeg", "AUDIO/X-WAV": "audio/wav", "audio/flac": "audio/flac", "audio/opus": "audio/opus"} {
		if got := NormalizeAudioMediaType(in); got != want {
			t.Errorf("NormalizeAudioMediaType(%q) = %q, want %q", in, got, want)
		}
	}
}

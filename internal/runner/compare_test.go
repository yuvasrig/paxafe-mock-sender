package runner

import (
	"strings"
	"testing"

	"github.com/yuvasrig/paxafe-mock-sender/internal/api"
)

func f(v float64) *float64 { return &v }

func TestSampleOfRequest(t *testing.T) {
	smp, ok := sampleOfRequest([]byte(`{"EntryTimeEpoch":1739215646000,"Temperature":{"Celsius":"5.25"},"Location":{"Latitude":40.81,"Longitude":-73.88}}`))
	if !ok || smp.TS != 1739215646000 || *smp.Temp != 5.25 || *smp.Lat != 40.81 || *smp.Lon != -73.88 {
		t.Fatalf("a numeric string counts as the number it holds: %+v %v", smp, ok)
	}
	if _, ok := sampleOfRequest([]byte(`not json`)); ok {
		t.Fatal("garbage has nothing to compare")
	}
	if _, ok := sampleOfRequest([]byte(`{"Humidity":{"Percentage":3}}`)); ok {
		t.Fatal("no temperature or position: nothing to compare")
	}
	if smp, _ := sampleOfRequest([]byte(`{"Temperature":{"Celsius":null},"Location":{"Latitude":"x"}}`)); smp.Temp != nil || smp.Lat != nil {
		t.Fatalf("%+v", smp)
	}
}

func TestCompare(t *testing.T) {
	sent := Sample{Temp: f(10.078125), Lat: f(40.8105625), Lon: f(-73.8792849)}
	cases := []struct {
		name    string
		sent    Sample
		got     *Sample
		verdict string
		detail  string
	}{
		{"stored as sent, within output rounding", sent, &Sample{Temp: f(10.08), Lat: f(40.810563), Lon: f(-73.879285)}, VerdictMatch, ""},
		{"temperature changed", sent, &Sample{Temp: f(11), Lat: f(40.810563), Lon: f(-73.879285)}, VerdictDiffers, "temperature sent 10.078125, stored 11"},
		{"coordinate moved", sent, &Sample{Temp: f(10.08), Lat: f(40.9), Lon: f(-73.879285)}, VerdictDiffers, "latitude"},
		{"temperature nulled by a rule", Sample{Temp: f(-32768), Lat: f(40.8), Lon: f(-73.8)}, &Sample{Lat: f(40.8), Lon: f(-73.8)}, VerdictDropped, "temperature not stored"},
		{"coordinates nulled by a rule", Sample{Temp: f(5), Lat: f(95), Lon: f(-73.8)}, &Sample{Temp: f(5)}, VerdictDropped, "latitude, longitude"},
		{"a change outranks a drop", Sample{Temp: f(5), Lat: f(40.8)}, &Sample{Temp: f(9)}, VerdictDiffers, "temperature"},
		{"nothing sent in a field is not compared", Sample{Temp: f(5)}, &Sample{Temp: f(5.001), Lat: f(1)}, VerdictMatch, ""},
		{"rejected: nothing stored", sent, nil, VerdictNotStored, "no sensor or location"},
	}
	for _, c := range cases {
		v, d := Compare(c.sent, c.got)
		if v != c.verdict || !strings.Contains(d, c.detail) {
			t.Errorf("%s: got %q %q, want %q containing %q", c.name, v, d, c.verdict, c.detail)
		}
	}
}

func TestAttachOnlyJudgesFinishedEvents(t *testing.T) {
	r := Result{Sent: &Sample{Temp: f(5)}}
	r.attach(api.EventInfo{Status: "pending"})
	if r.Compare != "" || r.Got != nil {
		t.Fatalf("a pending event has nothing to compare yet: %+v", r)
	}
	r.attach(api.EventInfo{Status: "accepted", HasProcessed: true, Temp: f(5)})
	if r.Compare != VerdictMatch || r.Got == nil {
		t.Fatalf("%+v", r)
	}
	r2 := Result{}
	r2.attach(api.EventInfo{Status: "accepted", HasProcessed: true})
	if r2.Compare != "" {
		t.Fatal("no sent values recorded: nothing to compare")
	}
}

func TestAFlaggedChangeIsFixedNotDifferent(t *testing.T) {
	r := Result{Sent: &Sample{Temp: f(-32768)}}
	r.attach(api.EventInfo{Status: "flagged", Flags: []string{"TEMPERATURE_FALLBACK"}, HasProcessed: true, Temp: f(5.1)})
	if r.Compare != VerdictFixed {
		t.Fatalf("a rule replaced the value and flagged it: %q", r.Compare)
	}
	r = Result{Sent: &Sample{Temp: f(5)}}
	r.attach(api.EventInfo{Status: "accepted", HasProcessed: true, Temp: f(9)})
	if r.Compare != VerdictDiffers {
		t.Fatalf("an unexplained change is a real difference: %q", r.Compare)
	}
}

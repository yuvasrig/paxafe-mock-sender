package runner

import (
	"encoding/json"
	"fmt"
	"math"
	"strconv"
	"strings"

	"github.com/yuvasrig/paxafe-mock-sender/internal/api"
)

// Sample is what we compare for one event: its time, temperature and position. A nil value was not present.
type Sample struct {
	TS   int64    `json:"ts"`
	Temp *float64 `json:"temp,omitempty"`
	Lat  *float64 `json:"lat,omitempty"`
	Lon  *float64 `json:"lon,omitempty"`
}

// Verdicts of comparing what was sent with what the API stored.
const (
	VerdictMatch     = "match"      // every value we sent was stored as sent (within the output rounding)
	VerdictDiffers   = "differs"    // a value was stored, but not as sent
	VerdictFixed     = "fixed"      // a value was stored differently, and the event carries a flag saying a rule did it
	VerdictDropped   = "dropped"    // a value we sent was not stored: the rules nulled it (and flagged it)
	VerdictNotStored = "not stored" // no sensor or location record exists (the event was rejected)
)

// Tolerances are the API's output rounding: temperature to 2 decimals, coordinates to 6.
const (
	tempTolerance  = 0.0051
	coordTolerance = 0.0000011
)

// sampleOfRequest reads the values we are about to compare out of the payload we sent. Numeric strings count as the
// numbers they hold, because the API coerces them.
func sampleOfRequest(body []byte) (Sample, bool) {
	var m map[string]any
	if json.Unmarshal(body, &m) != nil {
		return Sample{}, false
	}
	get := func(path ...string) *float64 {
		var v any = m
		for _, k := range path {
			obj, ok := v.(map[string]any)
			if !ok {
				return nil
			}
			v = obj[k]
		}
		switch t := v.(type) {
		case float64:
			return &t
		case string:
			if f, err := strconv.ParseFloat(strings.TrimSpace(t), 64); err == nil {
				return &f
			}
		}
		return nil
	}
	var smp Sample
	if ts := get("EntryTimeEpoch"); ts != nil {
		smp.TS = int64(*ts)
	}
	smp.Temp = get("Temperature", "Celsius")
	smp.Lat, smp.Lon = get("Location", "Latitude"), get("Location", "Longitude")
	return smp, smp.Temp != nil || smp.Lat != nil
}

// sampleOfEvent is what the API stored, or nil when it stored no record.
func sampleOfEvent(info api.EventInfo) *Sample {
	if !info.HasProcessed {
		return nil
	}
	return &Sample{TS: info.TS, Temp: info.Temp, Lat: info.Lat, Lon: info.Lon}
}

// Compare says whether what the API stored matches what was sent, value by value. It is pure: easy to test, and it
// is the whole definition of "matches" for the report.
func Compare(sent Sample, got *Sample) (verdict, detail string) {
	if got == nil {
		return VerdictNotStored, "no sensor or location record stored"
	}
	var differs, dropped []string
	check := func(name string, s, g *float64, tol float64) {
		switch {
		case s == nil:
		case g == nil:
			dropped = append(dropped, name)
		case math.Abs(*s-*g) > tol:
			differs = append(differs, fmt.Sprintf("%s sent %v, stored %v", name, *s, *g))
		}
	}
	check("temperature", sent.Temp, got.Temp, tempTolerance)
	check("latitude", sent.Lat, got.Lat, coordTolerance)
	check("longitude", sent.Lon, got.Lon, coordTolerance)
	switch {
	case len(differs) > 0:
		return VerdictDiffers, strings.Join(differs, "; ")
	case len(dropped) > 0:
		return VerdictDropped, strings.Join(dropped, ", ") + " not stored (nulled by a rule)"
	}
	return VerdictMatch, ""
}

// attach records what the API stored for the result's event and compares it with what was sent.
func (r *Result) attach(info api.EventInfo) {
	if r.Sent == nil || !terminal[info.Status] {
		return
	}
	r.Got = sampleOfEvent(info)
	r.Compare, r.CompareNote = Compare(*r.Sent, r.Got)
	if r.Compare == VerdictDiffers && len(info.Flags) > 0 {
		// A flagged event whose value changed is the rules doing their job (for example the probe temperature used when
		// the device temperature was a sentinel); only an unexplained change is a real difference.
		r.Compare = VerdictFixed
	}
}

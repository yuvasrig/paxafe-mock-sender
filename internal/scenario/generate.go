package scenario

import (
	"encoding/json"
	"fmt"
	"math"
	"math/rand"
	"time"
)

// A route from Boston to Atlanta with a known city every few points.
var waypoints = []struct {
	name, state, zip string
	lat, lon         float64
}{
	{"Boston", "MA", "02101", 42.3601, -71.0589},
	{"Hartford", "CT", "06103", 41.7658, -72.6734},
	{"New York", "NY", "10001", 40.7128, -74.0060},
	{"Philadelphia", "PA", "19103", 39.9526, -75.1652},
	{"Baltimore", "MD", "21201", 39.2904, -76.6122},
	{"Richmond", "VA", "23219", 37.5407, -77.4360},
	{"Raleigh", "NC", "27601", 35.7796, -78.6382},
	{"Charlotte", "NC", "28202", 35.2271, -80.8431},
	{"Atlanta", "GA", "30303", 33.7490, -84.3880},
}

// routeSpacing is the time between readings (Tive's minimum transmission interval).
const routeSpacing = 5 * time.Minute

// device is a simulated tracker with a fresh identity per run.
type device struct {
	imei, name string
}

func newDevice(seed int64) device {
	r := rand.New(rand.NewSource(seed))
	return device{
		imei: fmt.Sprintf("86%013d", r.Int63n(1e13)),
		name: fmt.Sprintf("SIM%05d", r.Intn(100000)),
	}
}

// point describes reading i of n on the route.
type point struct {
	i, n  int
	ts    time.Time
	lat   float64
	lon   float64
	place int // waypoint index of the nearest city
}

func routePoint(i, n int, end time.Time) point {
	frac := 0.0
	if n > 1 {
		frac = float64(i) / float64(n-1)
	}
	pos := frac * float64(len(waypoints)-1)
	a := int(math.Floor(pos))
	if a >= len(waypoints)-1 {
		a = len(waypoints) - 2
	}
	t := pos - float64(a)
	return point{
		i: i, n: n,
		ts:    end.Add(-time.Duration(n-1-i) * routeSpacing),
		lat:   waypoints[a].lat + (waypoints[a+1].lat-waypoints[a].lat)*t,
		lon:   waypoints[a].lon + (waypoints[a+1].lon-waypoints[a].lon)*t,
		place: int(math.Round(pos)),
	}
}

// payload builds a Tive payload as a map; callers tweak it before marshalling.
func payload(p Params, d device, pt point, tempC float64) map[string]any {
	wp := waypoints[pt.place]
	m := map[string]any{
		"EntityName":     d.name,
		"EntryTimeEpoch": pt.ts.UnixMilli(),
		"EntryTimeUtc":   pt.ts.UTC().Format("2006-01-02T15:04:05Z"),
		"Cellular":       map[string]any{"SignalStrength": "Good", "Dbm": -75 - pt.i%10},
		"Temperature":    map[string]any{"Celsius": round(tempC, 3), "Fahrenheit": round(tempC*9/5+32, 3)},
		"ProbeTemperature": map[string]any{
			"Celsius": round(tempC-0.1, 3), "Fahrenheit": round((tempC-0.1)*9/5+32, 3),
		},
		"Humidity":      map[string]any{"Percentage": round(45+3*math.Sin(float64(pt.i)/4), 1)},
		"Accelerometer": map[string]any{"G": 1.0, "X": 0.0, "Y": 0.0, "Z": 1.0},
		"Light":         map[string]any{"Lux": 0},
		"Battery":       map[string]any{"Percentage": maxInt(5, 95-pt.i), "Estimation": "Weeks", "IsCharging": false},
		"DeviceId":      d.imei,
		"DeviceName":    d.name,
		"Location": map[string]any{
			"Latitude": round(pt.lat, 6), "Longitude": round(pt.lon, 6),
			"FormattedAddress":         fmt.Sprintf("%s, %s %s, USA", wp.name, wp.state, wp.zip),
			"LocationMethod":           "gps",
			"Accuracy":                 map[string]any{"Meters": 5, "Kilometers": 0.005, "Miles": 0.003},
			"GeolocationSourceName":    "gnss",
			"CellTowerUsedCount":       0,
			"WifiAccessPointUsedCount": 0,
		},
	}
	if p.Account != 0 {
		m["AccountId"] = p.Account
	}
	return m
}

func marshal(m map[string]any) []byte {
	b, err := json.Marshal(m)
	if err != nil {
		panic(err) // maps of JSON-safe values cannot fail to marshal
	}
	return b
}

func round(f float64, places int) float64 {
	s := math.Pow10(places)
	return math.Round(f*s) / s
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

// inBand drifts gently inside the 2-8 C cold-chain band.
func inBand(i int) float64 { return 5 + 1.5*math.Sin(float64(i)/3) }

func points(p Params) int {
	if p.Points < 2 {
		return 12
	}
	return p.Points
}

func now(p Params) time.Time {
	if p.Now.IsZero() {
		return time.Now().UTC().Truncate(time.Second)
	}
	return p.Now
}

var okExpect = Expect{HTTP: []int{200}, Status: []string{"accepted"}, Ack: true}

func init() {
	register(Scenario{
		ID: "route", Title: "Realistic route",
		Description: "One tracker travels Boston to Atlanta, a reading every 5 minutes, GPS interpolated, temperature drifting inside 2-8 C. Everything should be accepted.",
		Build: func(p Params) []Step {
			d, n, end := newDevice(p.Seed), points(p), now(p)
			steps := make([]Step, n)
			for i := range steps {
				pt := routePoint(i, n, end)
				steps[i] = Step{Name: fmt.Sprintf("%s reading %d/%d", waypoints[pt.place].name, i+1, n), Body: marshal(payload(p, d, pt, inBand(i))), Expect: okExpect}
			}
			return steps
		},
	})

	register(Scenario{
		ID: "excursion", Title: "Temperature excursion",
		Description: "The same route, but the temperature climbs to 12 C. The excursion check is designed but not built, so the API still answers accepted with no TEMP_EXCURSION flag.",
		Build: func(p Params) []Step {
			d, n, end := newDevice(p.Seed), points(p), now(p)
			steps := make([]Step, n)
			for i := range steps {
				pt := routePoint(i, n, end)
				t := inBand(i)
				if i >= n/2 {
					t = 8 + 4*float64(i-n/2+1)/float64(n-n/2) // 8 -> 12 C
				}
				e := okExpect
				if t > 8 {
					e.Note = fmt.Sprintf("%.1f C is outside 2-8 C; TEMP_EXCURSION is not built, so no flag is expected", t)
				}
				steps[i] = Step{Name: fmt.Sprintf("%s %.1f C", waypoints[pt.place].name, t), Body: marshal(payload(p, d, pt, t)), Expect: e}
			}
			return steps
		},
	})

	register(Scenario{
		ID: "fault", Title: "Sensor fault",
		Description: "A route where the probe reports the sentinel values 327.67 and -32768, and in one reading the device temperature itself is faulty so the probe is used instead.",
		Build: func(p Params) []Step {
			d, n, end := newDevice(p.Seed), points(p), now(p)
			steps := make([]Step, n)
			for i := range steps {
				pt := routePoint(i, n, end)
				m := payload(p, d, pt, inBand(i))
				e, name := okExpect, fmt.Sprintf("%s ok", waypoints[pt.place].name)
				switch {
				case i%4 == 1:
					m["ProbeTemperature"] = map[string]any{"Celsius": 327.67, "Fahrenheit": 621.806}
					e = Expect{HTTP: []int{200}, Status: []string{"flagged"}, Flags: []string{"SENSOR_FAULT"}, ExactFlags: true, Ack: true, Note: "probe 327.67 nulled"}
					name = fmt.Sprintf("%s probe 327.67", waypoints[pt.place].name)
				case i%4 == 2:
					m["ProbeTemperature"] = map[string]any{"Celsius": -32768, "Fahrenheit": -58950.4}
					e = Expect{HTTP: []int{200}, Status: []string{"flagged"}, Flags: []string{"SENSOR_FAULT"}, ExactFlags: true, Ack: true, Note: "probe -32768 nulled"}
					name = fmt.Sprintf("%s probe -32768", waypoints[pt.place].name)
				case i == 3:
					m["Temperature"] = map[string]any{"Celsius": -32768, "Fahrenheit": -58950.4}
					e = Expect{HTTP: []int{200}, Status: []string{"flagged"}, Flags: []string{"SENSOR_FAULT", "TEMPERATURE_FALLBACK"}, ExactFlags: true, Ack: true, Note: "device temperature faulty: probe value used"}
					name = fmt.Sprintf("%s device temp -32768", waypoints[pt.place].name)
				}
				steps[i] = Step{Name: name, Body: marshal(m), Expect: e}
			}
			return steps
		},
	})

	register(Scenario{
		ID: "duplicates", Title: "Duplicates and conflicts",
		Description: "Each reading is sent twice. The repeat must come back as a duplicate. Every third reading is then sent a third time with one value changed, which must come back as a conflict (the first reading is kept).",
		Build: func(p Params) []Step {
			d, n, end := newDevice(p.Seed), minInt(points(p), 6), now(p)
			var steps []Step
			for i := 0; i < n; i++ {
				pt := routePoint(i, n, end)
				m := payload(p, d, pt, inBand(i))
				body := marshal(m)
				steps = append(steps,
					Step{Name: fmt.Sprintf("reading %d first send", i+1), Body: body, Expect: okExpect},
					Step{Name: fmt.Sprintf("reading %d repeat", i+1), Body: body, Expect: Expect{HTTP: []int{200}, Status: []string{"duplicate"}, Ack: true}},
				)
				if i%3 == 0 {
					m["Humidity"] = map[string]any{"Percentage": 99.9}
					steps = append(steps, Step{Name: fmt.Sprintf("reading %d changed", i+1), Body: marshal(m),
						Expect: Expect{HTTP: []int{200}, Status: []string{"conflict"}, Flags: []string{"DUPLICATE_CONFLICT"}, Ack: true, Note: "different content, same device and time"}})
				}
			}
			return steps
		},
	})

	register(Scenario{
		ID: "malformed", Title: "Malformed burst",
		Description: "Bad coordinates, numbers sent as strings, a timestamp in seconds, out-of-range values, missing identity, a timestamp in the future, and bodies that are not JSON. Each gets the documented answer; none may be lost.",
		Build:       malformedSteps,
	})

	register(Scenario{
		ID: "auth", Title: "Auth failures",
		Description: "Requests with no key, a key that does not exist, and something that is not a key at all. All must be 401, and nothing is stored (so there is nothing to look up afterwards).",
		Build: func(p Params) []Step {
			d, end := newDevice(p.Seed), now(p)
			body := marshal(payload(p, d, routePoint(0, 1, end), 5))
			e := Expect{HTTP: []int{401}, Note: "must not be stored"}
			return []Step{
				{Name: "no X-API-Key header", Body: body, Key: KeyNone, Expect: e},
				{Name: "well-formed but unknown key", Body: body, Key: KeyInvalid, Expect: e},
				{Name: "malformed key", Body: body, Key: KeyMalformed, Expect: e},
			}
		},
	})

	register(Scenario{
		ID: "load", Title: "Load burst", Load: true,
		Description: "Sends unique readings from many simulated trackers at a fixed rate for a fixed time, then reports throughput, error rate and latency percentiles. Run it from a laptop CLI for meaningful numbers.",
		Next: func(p Params, i int) Step {
			// 50 trackers take turns. A tracker's k-th reading is k intervals before Now, so no
			// (device, timestamp) pair repeats and nothing in the run is a duplicate.
			dev := newDevice(p.Seed + int64(i%50))
			pt := routePoint(i%12, 12, now(p))
			pt.ts = now(p).Add(-time.Duration(i/50) * routeSpacing)
			return Step{Name: fmt.Sprintf("load %d", i), Body: marshal(payload(p, dev, pt, inBand(i))), Expect: okExpect}
		},
	})
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func malformedSteps(p Params) []Step {
	d, end := newDevice(p.Seed), now(p)
	base := func(i int) map[string]any { return payload(p, d, routePoint(i, 12, end), inBand(i)) }
	var steps []Step
	add := func(name string, body []byte, e Expect) {
		steps = append(steps, Step{Name: name, Body: body, Expect: e})
	}
	flagged := func(note string, codes ...string) Expect {
		return Expect{HTTP: []int{200}, Status: []string{"flagged"}, Flags: codes, ExactFlags: true, Ack: true, Note: note}
	}

	m := base(0)
	m["Location"].(map[string]any)["Latitude"] = 95.0
	add("latitude 95", marshal(m), flagged("location skipped, sensor kept", "INVALID_COORDINATES"))

	m = base(1)
	m["Location"].(map[string]any)["Longitude"] = -200.0
	add("longitude -200", marshal(m), flagged("location skipped, sensor kept", "INVALID_COORDINATES"))

	m = base(2)
	m["Temperature"] = map[string]any{"Celsius": "5.25", "Fahrenheit": "41.45"}
	m["Cellular"] = map[string]any{"SignalStrength": "Good", "Dbm": "-80"}
	add("numbers as strings", marshal(m), Expect{HTTP: []int{200}, Status: []string{"accepted"}, Flags: []string{"COERCED_TYPE"}, Ack: true, Note: "parsed and flagged (info)"})

	m = base(3)
	m["EntryTimeEpoch"] = routePoint(3, 12, end).ts.Unix() // same instant as EntryTimeUtc, in seconds
	add("timestamp in seconds", marshal(m), flagged("multiplied by 1000", "TIMESTAMP_SECONDS"))

	m = base(4)
	m["Humidity"] = map[string]any{"Percentage": 150}
	add("humidity 150", marshal(m), flagged("humidity nulled", "OUT_OF_RANGE"))

	m = base(5)
	delete(m, "DeviceId")
	m["DeviceName"] = "NOT-A-KNOWN-DEVICE"
	m["EntityName"] = "NOT-A-KNOWN-DEVICE"
	add("no DeviceId, unknown name", marshal(m), Expect{HTTP: []int{422}, Status: []string{"rejected"}, Flags: nil, Ack: true, Rejection: true, Note: "NO_DEVICE_IDENTITY"})

	m = base(6)
	m["EntryTimeEpoch"] = end.Add(2 * time.Hour).UnixMilli()
	m["EntryTimeUtc"] = end.Add(2 * time.Hour).Format("2006-01-02T15:04:05Z")
	add("timestamp 2 h in the future", marshal(m), Expect{HTTP: []int{422}, Status: []string{"rejected"}, Ack: true, Rejection: true, Note: "TIMESTAMP_FUTURE"})

	m = base(7)
	delete(m, "Temperature")
	delete(m, "ProbeTemperature")
	m["Location"].(map[string]any)["Latitude"] = 95.0
	add("no temperature and bad coordinates", marshal(m), Expect{HTTP: []int{422}, Status: []string{"rejected"}, Ack: true, Rejection: true, Note: "NO_USABLE_DATA"})

	add("body is not JSON", []byte(`{this is not json`), Expect{HTTP: []int{400}, Status: []string{"rejected"}, Ack: true, Rejection: true, Note: "INVALID_JSON, raw body still archived"})
	add("body is a JSON array", []byte(`[1,2,3]`), Expect{HTTP: []int{400}, Status: []string{"rejected"}, Ack: true, Rejection: true, Note: "INVALID_JSON"})
	return steps
}

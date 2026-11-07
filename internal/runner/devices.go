package runner

import (
	"encoding/json"
	"sort"
	"strings"
)

// DeviceUse is one simulated device a run sent for: its ids, how many requests it sent, and (for a plan) which load and
// tenant it belonged to. The report lists them so the devices of a run can be found again in the API (the console's
// Payloads page filters by device name or IMEI).
type DeviceUse struct {
	IMEI         string `json:"imei"` // empty when the payload had no DeviceId (a deliberately malformed one)
	Name         string `json:"name"`
	Load         int    `json:"load,omitempty"`
	Target       string `json:"target,omitempty"` // the load's tenant label or key prefix
	Requests     int    `json:"requests"`
	Acknowledged int    `json:"acknowledged"`
}

// deviceOfRequest reads the device ids out of the payload that was sent.
func deviceOfRequest(body []byte) (imei, name string) {
	var m map[string]any
	if json.Unmarshal(body, &m) != nil {
		return "", ""
	}
	if s, ok := m["DeviceId"].(string); ok {
		imei = s
	}
	if s, ok := m["DeviceName"].(string); ok {
		name = s
	}
	return imei, name
}

// devicesOf lists the distinct devices of a run, per load, most requests first. A device used by two loads (two tenants)
// is listed once for each, since each tenant stored its own readings.
func devicesOf(results []Result, loads []LoadSummary) []DeviceUse {
	type key struct {
		load       int
		imei, name string
	}
	target := map[int]string{} // a load's number is its Index in the summary (a load that never started has none)
	for _, l := range loads {
		target[l.Index] = l.Target
	}
	byKey := map[key]*DeviceUse{}
	var order []key
	for _, r := range results {
		if r.DeviceIMEI == "" && r.DeviceName == "" {
			continue // a payload that was not a JSON object has no device to list
		}
		k := key{r.Load, r.DeviceIMEI, r.DeviceName}
		u := byKey[k]
		if u == nil {
			u = &DeviceUse{IMEI: r.DeviceIMEI, Name: r.DeviceName, Load: r.Load}
			u.Target = target[r.Load]
			byKey[k] = u
			order = append(order, k)
		}
		u.Requests++
		if r.EventID != "" {
			u.Acknowledged++
		}
	}
	out := make([]DeviceUse, 0, len(order))
	for _, k := range order {
		out = append(out, *byKey[k])
	}
	sort.SliceStable(out, func(i, j int) bool {
		switch {
		case out[i].Load != out[j].Load:
			return out[i].Load < out[j].Load
		case out[i].Requests != out[j].Requests:
			return out[i].Requests > out[j].Requests
		case (out[i].IMEI == "") != (out[j].IMEI == ""):
			return out[i].IMEI != "" // a device with ids before the payload that sent none
		}
		return strings.Compare(out[i].IMEI+out[i].Name, out[j].IMEI+out[j].Name) < 0
	})
	return out
}

package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"math"
	"strconv"
	"text/tabwriter"
	"time"

	"github.com/EsDmitrii/kconmon-ng/internal/model"
)

// humanizeDuration renders a time.Duration (as decoded from the API, i.e.
// nanoseconds) into a compact human string using µs / ms / s. Sub-microsecond
// values are shown in ns. Negative or zero durations render as "0s".
func humanizeDuration(d time.Duration) string {
	if d <= 0 {
		return "0s"
	}
	switch {
	case d < time.Microsecond:
		return strconv.FormatInt(int64(d), 10) + "ns"
	case d < time.Millisecond:
		return trimFloat(float64(d)/float64(time.Microsecond)) + "µs"
	case d < time.Second:
		return trimFloat(float64(d)/float64(time.Millisecond)) + "ms"
	default:
		return trimFloat(float64(d)/float64(time.Second)) + "s"
	}
}

// trimFloat formats a float with up to 2 decimals and trims trailing zeros.
func trimFloat(f float64) string {
	s := strconv.FormatFloat(f, 'f', 2, 64)
	// strip trailing zeros then a dangling dot
	for s != "" && s[len(s)-1] == '0' {
		s = s[:len(s)-1]
	}
	if s != "" && s[len(s)-1] == '.' {
		s = s[:len(s)-1]
	}
	return s
}

// humanizePct renders a loss ratio (0.0-1.0) as a percentage string.
func humanizePct(ratio float64) string {
	if math.IsNaN(ratio) || ratio < 0 {
		return "-"
	}
	return trimFloat(ratio*100) + "%"
}

// writeJSON pretty-prints raw JSON bytes; if they are not valid JSON it writes
// them verbatim so the operator always sees what the server returned.
func writeJSON(w io.Writer, raw []byte) error {
	var buf json.RawMessage
	if err := json.Unmarshal(raw, &buf); err != nil {
		_, werr := w.Write(raw)
		if werr == nil {
			_, werr = io.WriteString(w, "\n")
		}
		return werr
	}
	out, err := json.MarshalIndent(buf, "", "  ")
	if err != nil {
		return err
	}
	if _, werr := w.Write(out); werr != nil {
		return werr
	}
	_, err = io.WriteString(w, "\n")
	return err
}

func newTabWriter(w io.Writer) *tabwriter.Writer {
	return tabwriter.NewWriter(w, 0, 4, 3, ' ', 0)
}

// formatTopology renders the topology snapshot as a table joining nodes with
// their registered agent (if any).
func formatTopology(w io.Writer, snap *model.TopologySnapshot) error {
	if snap == nil {
		return nil
	}

	byNode := make(map[string]*model.AgentInfo, len(snap.Agents))
	for i := range snap.Agents {
		a := &snap.Agents[i]
		byNode[a.NodeName] = a
	}

	tw := newTabWriter(w)
	if _, err := fmt.Fprintln(tw, "NODE\tZONE\tREADY\tAGENT\tAGENT IP"); err != nil {
		return err
	}
	for _, n := range snap.Nodes {
		agent := "-"
		agentIP := "-"
		if a, ok := byNode[n.Name]; ok {
			agent = a.ID
			if a.PodIP != "" {
				agentIP = a.PodIP
			}
		}
		if _, err := fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n",
			orDash(n.Name), orDash(n.Zone), boolYesNo(n.Ready), agent, agentIP); err != nil {
			return err
		}
	}
	return tw.Flush()
}

// formatAgents renders the registered agents table.
func formatAgents(w io.Writer, snap *model.TopologySnapshot) error {
	if snap == nil {
		return nil
	}
	tw := newTabWriter(w)
	if _, err := fmt.Fprintln(tw, "ID\tNODE\tPOD IP\tZONE\tLAST SEEN"); err != nil {
		return err
	}
	for i := range snap.Agents {
		a := &snap.Agents[i]
		if _, err := fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n",
			orDash(a.ID), orDash(a.NodeName), orDash(a.PodIP), orDash(a.Zone),
			humanizeTime(a.LastSeen)); err != nil {
			return err
		}
	}
	return tw.Flush()
}

// formatCheck renders a one-shot diagnostic result as a human summary line plus
// the key numbers from its details block.
func formatCheck(w io.Writer, res *model.CheckResult) error {
	if res == nil {
		return nil
	}
	status := "OK"
	if !res.Success {
		status = "FAIL"
	}

	route := fmt.Sprintf("%s -> %s", orDash(res.Source), orDash(res.Destination))
	if res.SourceZone != "" || res.DestZone != "" {
		route += fmt.Sprintf(" (%s -> %s)", orDash(res.SourceZone), orDash(res.DestZone))
	}

	if _, err := fmt.Fprintf(w, "%s %s %s  duration=%s\n",
		status, string(res.Type), route, humanizeDuration(res.Duration)); err != nil {
		return err
	}
	if res.Error != "" {
		if _, err := fmt.Fprintf(w, "  error: %s\n", res.Error); err != nil {
			return err
		}
	}

	for _, line := range detailLines(res) {
		if _, err := fmt.Fprintf(w, "  %s\n", line); err != nil {
			return err
		}
	}
	return nil
}

// formatMTR renders the per-hop MTR table.
func formatMTR(w io.Writer, res *model.CheckResult) error {
	if res == nil {
		return nil
	}
	if res.Error != "" {
		if _, err := fmt.Fprintf(w, "error: %s\n", res.Error); err != nil {
			return err
		}
	}

	details, ok := decodeMTRDetails(res.Details)
	if !ok || details == nil {
		_, err := fmt.Fprintln(w, "no hops reported")
		return err
	}

	if details.Target != "" {
		if _, err := fmt.Fprintf(w, "target: %s\n", details.Target); err != nil {
			return err
		}
	}

	tw := newTabWriter(w)
	if _, err := fmt.Fprintln(tw, "HOP\tIP\tRTT\tLOSS"); err != nil {
		return err
	}
	silent := 0
	for _, hop := range details.Hops {
		// A hop that never answered the TTL-expired probe measured nothing:
		// showing the probe timeout as RTT would read as a (terrible) latency.
		if hop.IP == "" || hop.IP == "*" {
			silent++
			if _, err := fmt.Fprintf(tw, "%d\tno reply\t-\t-\n", hop.Number); err != nil {
				return err
			}
			continue
		}
		ip := hop.IP
		if hop.Hostname != "" {
			ip = fmt.Sprintf("%s (%s)", hop.IP, hop.Hostname)
		}
		if _, err := fmt.Fprintf(tw, "%d\t%s\t%s\t%s\n",
			hop.Number, ip, humanizeDuration(hop.RTT), humanizePct(hop.LossRatio)); err != nil {
			return err
		}
	}
	if err := tw.Flush(); err != nil {
		return err
	}

	// Closing verdict: what matters is whether the trace reached the target.
	if n := len(details.Hops); n > 0 {
		last := details.Hops[n-1]
		if last.IP != "" && last.IP != "*" && details.Target != "" && last.IP == details.Target {
			verdict := fmt.Sprintf("reached %s at hop %d (rtt %s)",
				details.Target, last.Number, humanizeDuration(last.RTT))
			if silent > 0 {
				verdict += fmt.Sprintf("; %d silent hop(s) — transit devices not answering TTL-expired probes, normal for bridges/overlays", silent)
			}
			_, err := fmt.Fprintln(w, verdict)
			return err
		}
		_, err := fmt.Fprintf(w, "target %s NOT reached within %d hops\n", orDash(details.Target), n)
		return err
	}
	return nil
}

// detailLines extracts the key numbers from a CheckResult for the human check
// summary. Every probe type speaks the same grammar — sent, recv, loss, rtt,
// then type-specific extras — so outputs stay comparable across types. TCP,
// DNS and HTTP run a single attempt per check, so their sent/recv are the
// honest 1/1 (or 1/0 with loss=100% on failure) derived from the result;
// UDP reports its real packet counts and ICMP its single echo. Decoding is
// defensive: unknown or malformed details yield no extra lines rather than
// panicking.
func detailLines(res *model.CheckResult) []string {
	if res == nil || res.Details == nil {
		return nil
	}
	raw, err := json.Marshal(res.Details)
	if err != nil {
		return nil
	}

	// Single-attempt probes: derive sent/recv/loss from the outcome.
	attempt := func() string {
		if res.Success {
			return "sent=1 recv=1 loss=0%"
		}
		return "sent=1 recv=0 loss=100%"
	}

	switch res.Type {
	case model.CheckICMP:
		var d model.ICMPDetails
		if json.Unmarshal(raw, &d) == nil {
			recv := 1
			if d.LossRatio > 0 {
				recv = 0
			}
			return []string{fmt.Sprintf("sent=1 recv=%d loss=%s rtt=%s",
				recv, humanizePct(d.LossRatio), humanizeDuration(d.RTT))}
		}
	case model.CheckUDP:
		var d model.UDPDetails
		if json.Unmarshal(raw, &d) == nil {
			return []string{fmt.Sprintf("sent=%d recv=%d loss=%s rtt=%s jitter=%s",
				d.PacketsSent, d.PacketsRecv, humanizePct(d.LossRatio),
				humanizeDuration(d.MeanRTT), humanizeDuration(d.Jitter))}
		}
	case model.CheckTCP:
		var d model.TCPDetails
		if json.Unmarshal(raw, &d) == nil {
			return []string{fmt.Sprintf("%s rtt=%s connect=%s",
				attempt(), humanizeDuration(d.TotalTime), humanizeDuration(d.ConnectTime))}
		}
	case model.CheckDNS:
		var d model.DNSDetails
		if json.Unmarshal(raw, &d) == nil {
			line := fmt.Sprintf("%s rtt=%s host=%s resolver=%s",
				attempt(), humanizeDuration(d.Duration), orDash(d.Host), orDash(d.Resolver))
			if len(d.ResolvedIPs) > 0 {
				ips := make([]string, 0, len(d.ResolvedIPs))
				for _, ip := range d.ResolvedIPs {
					ips = append(ips, ip.String())
				}
				line += " ips=" + joinComma(ips)
			}
			return []string{line}
		}
	case model.CheckHTTP:
		var d model.HTTPDetails
		if json.Unmarshal(raw, &d) == nil {
			return []string{fmt.Sprintf("%s rtt=%s ttfb=%s connect=%s status=%d",
				attempt(), humanizeDuration(d.TotalTime),
				humanizeDuration(d.TTFBTime), humanizeDuration(d.ConnectTime), d.StatusCode)}
		}
	case model.CheckMTR:
		if d, ok := decodeMTRDetails(res.Details); ok && d != nil {
			return []string{fmt.Sprintf("target=%s hops=%d", orDash(d.Target), len(d.Hops))}
		}
	default:
		return nil
	}
	return nil
}

// decodeMTRDetails defensively decodes a Details block into MTRDetails.
func decodeMTRDetails(details any) (*model.MTRDetails, bool) {
	if details == nil {
		return nil, false
	}
	raw, err := json.Marshal(details)
	if err != nil {
		return nil, false
	}
	var d model.MTRDetails
	if err := json.Unmarshal(raw, &d); err != nil {
		return nil, false
	}
	return &d, true
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

func boolYesNo(b bool) string {
	if b {
		return "yes"
	}
	return "no"
}

func humanizeTime(t time.Time) string {
	if t.IsZero() {
		return "-"
	}
	d := time.Since(t)
	if d < 0 {
		d = 0
	}
	return humanizeDuration(d) + " ago"
}

func joinComma(items []string) string {
	out := ""
	for i, s := range items {
		if i > 0 {
			out += ","
		}
		out += s
	}
	return out
}

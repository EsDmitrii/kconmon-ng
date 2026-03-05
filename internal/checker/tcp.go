package checker

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"time"

	"github.com/EsDmitrii/kconmon-ng/internal/model"
)

type TCPChecker struct {
	timeout time.Duration
	client  *http.Client
}

func NewTCPChecker(timeout time.Duration) *TCPChecker {
	dialer := &net.Dialer{Timeout: timeout}
	transport := &http.Transport{
		DialContext:           dialer.DialContext,
		DisableKeepAlives:     true,
		MaxIdleConns:          0,
		ResponseHeaderTimeout: timeout,
	}

	return &TCPChecker{
		timeout: timeout,
		client:  &http.Client{Transport: transport, Timeout: timeout},
	}
}

func (c *TCPChecker) Name() model.CheckType {
	return model.CheckTCP
}

func (c *TCPChecker) Check(ctx context.Context, target Target) model.CheckResult {
	result := model.CheckResult{
		Type:      model.CheckTCP,
		Timestamp: time.Now(),
	}

	addr := net.JoinHostPort(target.PodIP, strconv.Itoa(target.Port))

	dialer := net.Dialer{Timeout: c.timeout}
	connectStart := time.Now()
	conn, err := dialer.DialContext(ctx, "tcp", addr)
	connectDuration := time.Since(connectStart)

	if err != nil {
		result.Error = fmt.Sprintf("TCP connect failed: %v", err)
		result.Duration = connectDuration
		return result
	}
	_ = conn.Close()

	url := fmt.Sprintf("http://%s/readyz", addr)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, http.NoBody)
	if err != nil {
		result.Error = fmt.Sprintf("creating request: %v", err)
		return result
	}
	req.Header.Set("Connection", "close")

	totalStart := time.Now()
	resp, err := c.client.Do(req) //nolint:gosec // G704: SSRF by design — checker probes known pod IPs
	totalDuration := time.Since(totalStart)

	if err != nil {
		result.Error = fmt.Sprintf("HTTP readiness check failed: %v", err)
		result.Duration = totalDuration
		result.Details = &model.TCPDetails{
			ConnectTime: connectDuration,
			TotalTime:   totalDuration,
		}
		return result
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, resp.Body)

	result.Success = resp.StatusCode == http.StatusOK
	result.Duration = totalDuration
	result.Details = &model.TCPDetails{
		ConnectTime: connectDuration,
		TotalTime:   totalDuration,
	}

	if !result.Success {
		result.Error = fmt.Sprintf("unexpected status: %d", resp.StatusCode)
	}

	return result
}

package cache

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/redis/rueidis"
)

// redis6 is a minimal RESP server with Redis 6.2 command arity: PEXPIRE takes exactly key and
// milliseconds, so the NX/XX/GT/LT options added in 7.0 are an error, as on a real 6.x server.
type redis6 struct {
	mu      sync.Mutex
	vals    map[string]int64
	strs    map[string]string
	expires map[string]time.Time
	// refuse holds commands answered as unknown, the way a Redis 5 server answers HELLO or a
	// managed endpoint answers a renamed-away CLIENT.
	refuse map[string]bool
}

func startRedis6(t *testing.T, refuse ...string) string {
	t.Helper()
	srv := &redis6{vals: map[string]int64{}, strs: map[string]string{}, expires: map[string]time.Time{}, refuse: map[string]bool{}}
	for _, c := range refuse {
		srv.refuse[c] = true
	}
	lc := net.ListenConfig{}
	lis, err := lc.Listen(context.Background(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = lis.Close() })
	go func() {
		for {
			conn, acceptErr := lis.Accept()
			if acceptErr != nil {
				return
			}
			go srv.serve(conn)
		}
	}()
	return lis.Addr().String()
}

// newRedis6Client connects to a startRedis6 server the way the console does: one connection, no
// client-side cache. The caller closes it.
func newRedis6Client(t *testing.T, addr string) rueidis.Client {
	t.Helper()
	client, err := rueidis.NewClient(rueidis.ClientOption{
		InitAddress:       []string{addr},
		ForceSingleClient: true,
		DisableCache:      true,
	})
	if err != nil {
		t.Fatalf("rueidis.NewClient: %v", err)
	}
	return client
}

func (s *redis6) serve(conn net.Conn) {
	defer func() { _ = conn.Close() }()
	r := bufio.NewReader(conn)
	for {
		args, err := readCommand(r)
		if err != nil {
			return
		}
		if _, err := io.WriteString(conn, s.reply(args)); err != nil {
			return
		}
	}
}

func readCommand(r *bufio.Reader) ([]string, error) {
	line, err := r.ReadString('\n')
	if err != nil {
		return nil, err
	}
	n, err := strconv.Atoi(strings.TrimSpace(strings.TrimPrefix(line, "*")))
	if err != nil {
		return nil, err
	}
	args := make([]string, 0, n)
	for range n {
		hdr, err := r.ReadString('\n')
		if err != nil {
			return nil, err
		}
		size, err := strconv.Atoi(strings.TrimSpace(strings.TrimPrefix(hdr, "$")))
		if err != nil {
			return nil, err
		}
		buf := make([]byte, size+2)
		if _, err := io.ReadFull(r, buf); err != nil {
			return nil, err
		}
		args = append(args, string(buf[:size]))
	}
	return args, nil
}

func (s *redis6) reply(args []string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := ""
	if len(args) > 1 {
		key = args[1]
	}
	if exp, ok := s.expires[key]; ok && time.Now().After(exp) {
		delete(s.vals, key)
		delete(s.strs, key)
		delete(s.expires, key)
	}
	if s.refuse[strings.ToUpper(args[0])] {
		return fmt.Sprintf("-ERR unknown command '%s', with args beginning with: \r\n", args[0])
	}
	switch strings.ToUpper(args[0]) {
	case "HELLO":
		return "%3\r\n+server\r\n+redis\r\n+version\r\n+6.2.14\r\n+proto\r\n:3\r\n"
	case "CLIENT":
		return "+OK\r\n"
	case "PING":
		return "+PONG\r\n"
	case "INCR":
		s.vals[key]++
		return fmt.Sprintf(":%d\r\n", s.vals[key])
	case "PTTL":
		_, isInt := s.vals[key]
		_, isStr := s.strs[key]
		if !isInt && !isStr {
			return ":-2\r\n"
		}
		exp, ok := s.expires[key]
		if !ok {
			return ":-1\r\n"
		}
		return fmt.Sprintf(":%d\r\n", time.Until(exp).Milliseconds())
	case "PEXPIRE":
		if len(args) != 3 {
			return "-ERR wrong number of arguments for 'pexpire' command\r\n"
		}
		ms, err := strconv.ParseInt(args[2], 10, 64)
		if err != nil {
			return "-ERR value is not an integer or out of range\r\n"
		}
		if _, ok := s.vals[key]; !ok {
			return ":0\r\n"
		}
		s.expires[key] = time.Now().Add(time.Duration(ms) * time.Millisecond)
		return ":1\r\n"
	case "SET":
		return s.set(args)
	case "GET":
		v, ok := s.strs[key]
		if !ok {
			return "_\r\n"
		}
		return fmt.Sprintf("$%d\r\n%s\r\n", len(v), v)
	default:
		return fmt.Sprintf("-ERR unknown command '%s'\r\n", args[0])
	}
}

// set is SET key value [NX|XX] [PX ms], the subset RedisKV sends.
func (s *redis6) set(args []string) string {
	if len(args) < 3 {
		return "-ERR wrong number of arguments for 'set' command\r\n"
	}
	key, val := args[1], args[2]
	_, exists := s.strs[key]
	var ttl time.Duration
	for i := 3; i < len(args); i++ {
		switch strings.ToUpper(args[i]) {
		case "NX":
			if exists {
				return "_\r\n"
			}
		case "XX":
			if !exists {
				return "_\r\n"
			}
		case "PX":
			if i+1 >= len(args) {
				return "-ERR syntax error\r\n"
			}
			ms, err := strconv.ParseInt(args[i+1], 10, 64)
			if err != nil {
				return "-ERR value is not an integer or out of range\r\n"
			}
			ttl = time.Duration(ms) * time.Millisecond
			i++
		default:
			return "-ERR syntax error\r\n"
		}
	}
	s.strs[key] = val
	delete(s.expires, key)
	if ttl > 0 {
		s.expires[key] = time.Now().Add(ttl)
	}
	return "+OK\r\n"
}

// SetNX and SetXX map to SET's NX and XX options; a refused one is Valkey's nil reply, not an error.
func TestRedisKVSetNXAndSetXX(t *testing.T) {
	addr := startRedis6(t)
	client := newRedis6Client(t, addr)
	t.Cleanup(client.Close)
	kv := &RedisKV{client: client}
	ctx := t.Context()

	if ok, err := kv.SetXX(ctx, "sess:a", []byte("x"), time.Minute); err != nil || ok {
		t.Fatalf("SetXX on an absent key = %v, %v; want false, nil", ok, err)
	}
	if ok, err := kv.SetNX(ctx, "sess:a", []byte("one"), time.Minute); err != nil || !ok {
		t.Fatalf("SetNX on an absent key = %v, %v; want true, nil", ok, err)
	}
	if ok, err := kv.SetNX(ctx, "sess:a", []byte("two"), time.Minute); err != nil || ok {
		t.Fatalf("SetNX on a live key = %v, %v; want false, nil", ok, err)
	}
	if ok, err := kv.SetXX(ctx, "sess:a", []byte("three"), time.Minute); err != nil || !ok {
		t.Fatalf("SetXX on a live key = %v, %v; want true, nil", ok, err)
	}
	if val, found, err := kv.Get(ctx, "sess:a"); err != nil || !found || string(val) != "three" {
		t.Errorf("Get = %q, %v, %v; want %q", val, found, err, "three")
	}
	if _, ok := srvTTL(t, addr, "sess:a"); !ok {
		t.Error("SetXX left the key without a TTL")
	}
}

// IncrWithTTL must arm the window on a server without PEXPIRE NX (Redis 6.x): the chart accepts
// any Redis-compatible server, and a counter without a TTL never resets.
func TestRedisKVIncrWithTTLWorksOnRedis6(t *testing.T) {
	addr := startRedis6(t)
	client := newRedis6Client(t, addr)
	t.Cleanup(client.Close)
	kv := &RedisKV{client: client}
	ctx := t.Context()

	for want := int64(1); want <= 3; want++ {
		n, err := kv.IncrWithTTL(ctx, "rl:login:u:alice", time.Minute)
		if err != nil {
			t.Fatalf("IncrWithTTL #%d: %v", want, err)
		}
		if n != want {
			t.Fatalf("IncrWithTTL #%d = %d, want %d", want, n, want)
		}
	}
	exp, ok := srvTTL(t, addr, "rl:login:u:alice")
	if !ok {
		t.Fatal("the counter has no TTL, so it would never expire")
	}
	if left := time.Until(exp); left <= 0 || left > time.Minute {
		t.Errorf("TTL left = %v, want within (0, 1m]", left)
	}
}

// srvTTL reads the fake's expiry through the wire, as a client would.
func srvTTL(t *testing.T, addr, key string) (time.Time, bool) {
	t.Helper()
	client := newRedis6Client(t, addr)
	defer client.Close()
	ms, err := client.Do(t.Context(), client.B().Pttl().Key(key).Build()).AsInt64()
	if err != nil {
		t.Fatalf("PTTL: %v", err)
	}
	if ms < 0 {
		return time.Time{}, false
	}
	return time.Now().Add(time.Duration(ms) * time.Millisecond), true
}

// A session key carries the bearer session id, and callers log these errors: no RedisKV error may
// repeat the key past its prefix.
func TestRedisKVErrorsDoNotCarryTheKey(t *testing.T) {
	addr := startRedis6(t)
	client := newRedis6Client(t, addr)
	kv := &RedisKV{client: client}
	client.Close() // every command now fails
	ctx := t.Context()
	const secret = "Zq3xS3cr3tS3ssionId"
	key := "sess:" + secret

	errs := map[string]error{}
	_, _, errs["get"] = kv.Get(ctx, key)
	errs["set"] = kv.Set(ctx, key, []byte("v"), time.Minute)
	_, errs["setnx"] = kv.SetNX(ctx, key, []byte("v"), time.Minute)
	_, errs["setxx"] = kv.SetXX(ctx, key, []byte("v"), time.Minute)
	errs["delete"] = kv.Delete(ctx, key)
	_, errs["deleteifequal"] = kv.DeleteIfEqual(ctx, key, []byte("v"))
	_, errs["incr"] = kv.IncrWithTTL(ctx, key, time.Minute)
	for op, err := range errs {
		if err == nil {
			t.Errorf("%s on a closed client returned no error", op)
			continue
		}
		if strings.Contains(err.Error(), secret) {
			t.Errorf("%s error carries the raw key: %q", op, err)
		}
		if !strings.Contains(err.Error(), logKey(key)) {
			t.Errorf("%s error = %q, want it to name the key as %q so it matches the other ops' errors", op, err, logKey(key))
		}
	}
}

// NewRedisBus must work against a server without HELLO (RESP2-only, Redis 5) or CLIENT (renamed away
// on some managed endpoints): the console never reads through client-side caching.
func TestNewRedisBusDoesNotNeedClientSideCaching(t *testing.T) {
	for _, refused := range []string{"HELLO", "CLIENT"} {
		addr := startRedis6(t, refused)
		bus, err := NewRedisBus(t.Context(), "redis://"+addr, time.Second)
		if err != nil {
			t.Errorf("NewRedisBus against a server without %s: %v", refused, err)
			continue
		}
		n, err := NewRedisKVFromBus(bus).IncrWithTTL(t.Context(), "rl:x", time.Minute)
		bus.Close()
		if err != nil || n != 1 {
			t.Errorf("IncrWithTTL against a server without %s = %d, %v; want 1, nil", refused, n, err)
		}
	}
}

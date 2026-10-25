package service

import (
	"context"
	"fmt"
	"net"
	"testing"

	"github.com/cytusc/node_list/backend/internal/domain"
)

func TestRejectUnreachableEndpointsDropsOnlyDeadPorts(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	live := fmt.Sprint(ln.Addr().(*net.TCPAddr).Port)

	nodes := []domain.Node{
		{Type: "http", Server: "127.0.0.1", Port: live},
		{Type: "http", Server: "127.0.0.1", Port: "1"},
		{Type: "hysteria2", Server: "127.0.0.1", Port: "1"},
	}
	denied, err := f.repo.RejectUnreachableEndpoints(ctx, nodes)
	if err != nil {
		t.Fatal(err)
	}
	if denied[1] != "endpoint-unreachable" {
		t.Fatalf("closed port not rejected: %v", denied)
	}
	// A listening port passes; a UDP transport is never dialled over TCP.
	if _, ok := denied[0]; ok {
		t.Fatalf("reachable endpoint rejected: %v", denied)
	}
	if _, ok := denied[2]; ok {
		t.Fatalf("udp transport rejected by a tcp dial: %v", denied)
	}
}

func TestRejectUnreachableEndpointsYieldsWhenTheProbePathIsBroken(t *testing.T) {
	f := setup(t)
	// Everything looks dead. That is far more likely a broken probe path than a
	// fully dead batch, and silencing the source would be worse than admitting it.
	nodes := []domain.Node{
		{Type: "http", Server: "127.0.0.1", Port: "1"},
		{Type: "http", Server: "127.0.0.1", Port: "2"},
		{Type: "http", Server: "127.0.0.1", Port: "3"},
	}
	denied, err := f.repo.RejectUnreachableEndpoints(context.Background(), nodes)
	if err != nil {
		t.Fatal(err)
	}
	if len(denied) != 0 {
		t.Fatalf("whole-batch rejection must yield: %v", denied)
	}
}

func TestRejectUnreachableEndpointsCanBeDisabled(t *testing.T) {
	f := setup(t)
	t.Setenv("SOURCE_PROBE_SECONDS", "0")
	denied, err := f.repo.RejectUnreachableEndpoints(context.Background(), []domain.Node{
		{Type: "http", Server: "127.0.0.1", Port: "1"},
	})
	if err != nil || len(denied) != 0 {
		t.Fatalf("disabled probe still filtered: %v %v", denied, err)
	}
}

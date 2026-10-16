package mcp

import (
	"context"
	"strings"
	"testing"
)

func TestPrivateMemoryToolsOnlyAppearOnLocalGateway(t *testing.T) {
	request := []byte(`{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{}}`)
	local := New(Options{Project: "demo"})
	if !strings.Contains(string(local.Dispatch(context.Background(), request)), `"memory_search"`) {
		t.Fatal("local gateway did not expose memory search")
	}
	remote := newHTTP("http://127.0.0.1", "token", "demo", "")
	if strings.Contains(string(remote.Dispatch(context.Background(), request)), `"memory_search"`) {
		t.Fatal("HTTP gateway exposed private memory tools")
	}
	call := []byte(`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"memory_search","arguments":{}}}`)
	if !strings.Contains(string(remote.Dispatch(context.Background(), call)), "local stdio gateway") {
		t.Fatal("HTTP gateway accepted a private memory call")
	}
}

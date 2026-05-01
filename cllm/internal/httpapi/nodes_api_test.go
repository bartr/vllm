package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"cllm/internal/node"
)

func TestNodesAPIListCreateUpdateDelete(t *testing.T) {
	handler := NewHandler()
	handler.SetNodes([]*node.Node{makeTestNode("vllm", "vllm", 100000)}, "least-loaded")
	routes := handler.Routes()

	createBody := `{
		"class":"synthetic",
		"max_tokens_in_flight":200000,
		"max_waiting_requests":32,
		"per_request_tokens_per_second":64,
		"degradation_threshold":10,
		"max_concurrency":128,
		"max_degradation":60,
		"prefill_rate_multiplier":10
	}`
	createRec := httptest.NewRecorder()
	createReq := httptest.NewRequest(http.MethodPost, "/nodes/cllm", strings.NewReader(createBody))
	createReq.Header.Set("Content-Type", "application/json")
	routes.ServeHTTP(createRec, createReq)
	if createRec.Code != http.StatusCreated {
		t.Fatalf("create status = %d body = %q", createRec.Code, createRec.Body.String())
	}

	if got := strings.Join(handler.NodeIDs(), ","); got != "cllm,vllm" {
		t.Fatalf("NodeIDs after create = %q, want cllm,vllm", got)
	}
	createdNode, ok := handler.getNodeSnapshot("cllm")
	if !ok {
		t.Fatalf("created node not found")
	}
	if _, ok := createdNode.Budget.Acquire(reqContext(t), 100); !ok {
		t.Fatalf("failed to acquire initial node budget")
	}

	updateRec := httptest.NewRecorder()
	updateReq := httptest.NewRequest(http.MethodPost, "/nodes/cllm?per-request-tokens-per-second=96&bypass-cache=true", nil)
	routes.ServeHTTP(updateRec, updateReq)
	if updateRec.Code != http.StatusOK {
		t.Fatalf("update status = %d body = %q", updateRec.Code, updateRec.Body.String())
	}

	var updated nodeResponse
	if err := json.Unmarshal(updateRec.Body.Bytes(), &updated); err != nil {
		t.Fatalf("decode update response: %v", err)
	}
	if updated.Capacity.PerRequestTPS != 96 {
		t.Fatalf("PerRequestTPS = %d, want 96", updated.Capacity.PerRequestTPS)
	}
	if !updated.Capacity.BypassCache {
		t.Fatalf("BypassCache = false, want true")
	}
	updatedNode, ok := handler.getNodeSnapshot("cllm")
	if !ok {
		t.Fatalf("updated node not found")
	}
	if updatedNode != createdNode {
		t.Fatalf("update replaced node pointer; want in-place reconfiguration")
	}
	_, inFlight, _, _ := updatedNode.Budget.Stats()
	if inFlight != 100 {
		t.Fatalf("budget in-flight after update = %d, want 100", inFlight)
	}

	listRec := httptest.NewRecorder()
	routes.ServeHTTP(listRec, httptest.NewRequest(http.MethodGet, "/nodes", nil))
	if listRec.Code != http.StatusOK {
		t.Fatalf("list status = %d body = %q", listRec.Code, listRec.Body.String())
	}
	var list nodesResponse
	if err := json.Unmarshal(listRec.Body.Bytes(), &list); err != nil {
		t.Fatalf("decode list response: %v", err)
	}
	if list.Count != 2 || list.RouterPolicy != "least-loaded" {
		t.Fatalf("list = %+v, want count=2 policy=least-loaded", list)
	}

	deleteRec := httptest.NewRecorder()
	routes.ServeHTTP(deleteRec, httptest.NewRequest(http.MethodDelete, "/nodes/cllm", nil))
	if deleteRec.Code != http.StatusOK {
		t.Fatalf("delete status = %d body = %q", deleteRec.Code, deleteRec.Body.String())
	}
	if got := strings.Join(handler.NodeIDs(), ","); got != "vllm" {
		t.Fatalf("NodeIDs after delete = %q, want vllm", got)
	}
}

func reqContext(t *testing.T) context.Context {
	t.Helper()
	return context.Background()
}

func TestNodesAPICannotDeleteLastNode(t *testing.T) {
	handler := NewHandler()
	handler.SetNodes([]*node.Node{makeTestNode("only", "default", 100000)}, "")
	routes := handler.Routes()

	rec := httptest.NewRecorder()
	routes.ServeHTTP(rec, httptest.NewRequest(http.MethodDelete, "/nodes/only", nil))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d body = %q, want 400", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "cannot delete the last node") {
		t.Fatalf("body = %q, want last-node error", rec.Body.String())
	}
	if got := strings.Join(handler.NodeIDs(), ","); got != "only" {
		t.Fatalf("NodeIDs = %q, want only", got)
	}
}

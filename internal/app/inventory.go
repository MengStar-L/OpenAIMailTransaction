package app

import (
	"context"
	"errors"
	"net/http"
	"openai-mail-transaction/internal/provider"
	"time"
)

type emailInventoryView struct {
	Count               *int       `json:"count"`
	Status              string     `json:"status"`
	UpdatedAt           *time.Time `json:"updated_at"`
	Message             string     `json:"message"`
	AllocationStatus    string     `json:"allocation_status"`
	AllocationCheckedAt *time.Time `json:"allocation_checked_at"`
}

const allocationObservationTTL = 2 * time.Minute
const maxAllocationObservations = 128

type allocationObservation struct {
	CheckedAt time.Time
}

func emailInventoryKey(req provider.Request, apiKey string) [4]string {
	return [4]string{req.Service, req.Domain, req.MaxPrice, hash(apiKey)}
}

// The caller holds providerGate, so the credential cannot change between the
// purchase and this observation. Read it before inventoryMu to keep lock order
// consistent with inventory queries; never hold inventoryMu during a purchase.
func (a *App) observeEmailAllocation(req provider.Request, allocationErr error) {
	if req.Kind != "email" {
		return
	}
	key := emailInventoryKey(req, a.currentAPIKey())
	now := time.Now().UTC()
	a.inventoryMu.Lock()
	defer a.inventoryMu.Unlock()
	if a.inventoryCache.Key == key {
		a.inventoryCache = inventoryCache{}
	}
	a.pruneAllocationObservations(now)
	if allocationErr == nil {
		delete(a.allocationObservations, key)
		return
	}
	var pe *provider.Error
	if !errors.As(allocationErr, &pe) || pe.Uncertain || pe.Code != "no_stock" {
		return
	}
	if a.allocationObservations == nil {
		a.allocationObservations = make(map[[4]string]allocationObservation)
	}
	if _, exists := a.allocationObservations[key]; !exists && len(a.allocationObservations) >= maxAllocationObservations {
		var oldestKey [4]string
		var oldest time.Time
		for candidate, observation := range a.allocationObservations {
			if oldest.IsZero() || observation.CheckedAt.Before(oldest) {
				oldestKey, oldest = candidate, observation.CheckedAt
			}
		}
		delete(a.allocationObservations, oldestKey)
	}
	a.allocationObservations[key] = allocationObservation{CheckedAt: now}
}

// These helpers only run while inventoryMu is held. The cache stores the
// upstream result alone; a new quote cannot erase a recent allocation refusal.
func (a *App) pruneAllocationObservations(now time.Time) {
	for key, observation := range a.allocationObservations {
		if !now.Before(observation.CheckedAt.Add(allocationObservationTTL)) {
			delete(a.allocationObservations, key)
		}
	}
}

func (a *App) withAllocationObservation(key [4]string, view emailInventoryView) emailInventoryView {
	a.pruneAllocationObservations(time.Now().UTC())
	view.AllocationStatus, view.AllocationCheckedAt = "unconfirmed", nil
	if observation, exists := a.allocationObservations[key]; exists {
		view.AllocationStatus = "no_stock"
		view.AllocationCheckedAt = &observation.CheckedAt
		view.Message = "最近分配被上游拒绝，当前条件下暂无可分配邮箱"
	}
	return view
}

type inventoryCache struct {
	Key       [4]string
	ExpiresAt time.Time
	Email     emailInventoryView
}

// Both page types share a bounded, read-only provider lookup. A successful zero
// is distinct from an unavailable result; failed refreshes never masquerade as
// empty stock or keep presenting a stale count as current.
func (a *App) inventory(w http.ResponseWriter, r *http.Request) {
	a.providerGate.RLock()
	defer a.providerGate.RUnlock()
	s := a.currentSettings()
	view := emailInventoryView{Status: "unconfigured", Message: "邮箱服务尚未配置", AllocationStatus: "unconfirmed"}
	if !a.providerReady() {
		a.inventoryResponse(w, view)
		return
	}
	if a.opts.Mode == "live" && (s.EmailService == "" || s.EmailDomain == "" || s.EmailMaxPrice == "") {
		view.Message = "邮箱参数尚未配置"
		a.inventoryResponse(w, view)
		return
	}
	client, ok := a.currentClient().(provider.InventoryClient)
	if !ok {
		view.Status, view.Message = "unavailable", "暂时无法读取邮箱库存"
		a.inventoryResponse(w, view)
		return
	}
	request := provider.Request{Kind: "email", Service: s.EmailService, Domain: s.EmailDomain, MaxPrice: s.EmailMaxPrice}
	key := emailInventoryKey(request, a.currentAPIKey())
	a.inventoryMu.Lock()
	defer a.inventoryMu.Unlock()
	if a.inventoryCache.Key == key && time.Now().Before(a.inventoryCache.ExpiresAt) {
		a.inventoryResponse(w, a.withAllocationObservation(key, a.inventoryCache.Email))
		return
	}
	ctx, cancel := context.WithTimeout(a.ctx, 8*time.Second)
	stock, err := client.EmailInventory(ctx, request)
	cancel()
	ttl := 10 * time.Second
	view.Status, view.Message = "unavailable", "暂时无法读取邮箱库存"
	if err == nil && stock.Count >= 0 {
		now := time.Now().UTC()
		view = emailInventoryView{Count: &stock.Count, Status: "available", UpdatedAt: &now, Message: "上游参考库存，实际以分配结果为准", AllocationStatus: "unconfirmed"}
		ttl = 30 * time.Second
	}
	a.inventoryCache = inventoryCache{Key: key, ExpiresAt: time.Now().Add(ttl), Email: view}
	a.inventoryResponse(w, a.withAllocationObservation(key, view))
}

func (a *App) inventoryResponse(w http.ResponseWriter, view emailInventoryView) {
	respond(w, http.StatusOK, map[string]any{"email": view, "server_time": time.Now().UTC()})
}

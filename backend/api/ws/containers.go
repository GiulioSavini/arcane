package ws

import (
	"context"
	"encoding/json/v2"
	"log/slog"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"
	containertypes "github.com/getarcaneapp/arcane/types/v2/container"
	systemtypes "github.com/getarcaneapp/arcane/types/v2/system"
	"github.com/labstack/echo/v5"
	kit "go.getarcane.app/kit/pkg"

	wshub "github.com/getarcaneapp/arcane/backend/v2/pkg/libarcane/ws"
)

const (
	// containersStatsInterval is the pause between batches and the oldest cached sample a batch reuses.
	containersStatsInterval = 2 * time.Second
	// containersStatsReadLimit fits a subscription of 500 full container IDs, the most one batch samples.
	containersStatsReadLimit = 64 << 10
)

// ContainersStats streams resource samples for a client-chosen set of containers over one WebSocket.
// The client sends a ResourceSampleSubscription whenever its set changes; the server answers with a
// ResourceSampleBatch every couple of seconds.
//
//	@Summary		Get batched container stats via WebSocket
//	@Description	Stream resource samples for the subscribed containers over one WebSocket connection
//	@Tags			WebSocket
//	@Param			id	path	string	true	"Environment ID"
//	@Router			/api/environments/{id}/ws/containers/stats [get]
func (h *WebSocketHandler) ContainersStats(c *echo.Context) error {
	conn, unregister, ok := h.acceptWS(c, systemtypes.WSKindContainerStats, "batch")
	if !ok {
		return nil
	}
	defer unregister()
	defer func() { _ = conn.CloseNow() }()
	conn.SetReadLimit(containersStatsReadLimit)

	ctx, cancel := context.WithCancel(c.Request().Context())
	defer cancel()

	var ids atomic.Pointer[[]string]
	wake := make(chan struct{}, 1)
	go readContainersStatsPump(ctx, cancel, conn, &ids, wake)
	// The pong is serviced by readContainersStatsPump.
	go wshub.KeepAlive(ctx, cancel, conn)

	send := func(batch containertypes.ResourceSampleBatch) error {
		b, err := json.Marshal(batch)
		if err != nil {
			return err
		}
		wctx, wcancel := context.WithTimeout(ctx, 10*time.Second)
		defer wcancel()
		return conn.Write(wctx, websocket.MessageText, b)
	}

	// Nothing to sample until the first subscription arrives.
	select {
	case <-ctx.Done():
		return nil
	case <-wake:
	}

	for {
		samples, err := h.containerService.SampleResources(ctx, kit.FromPtr(ids.Load()), containersStatsInterval)
		switch {
		case ctx.Err() != nil:
			return nil
		case err != nil:
			slog.DebugContext(ctx, "Failed to sample container stats", "envId", c.Param("id"), "error", err)
			if send(containertypes.ResourceSampleBatch{Error: "Failed to sample container stats: " + err.Error(), Timestamp: time.Now()}) != nil {
				return nil
			}
		default:
			if send(containertypes.ResourceSampleBatch{Samples: samples, Timestamp: time.Now()}) != nil {
				return nil
			}
		}

		select {
		case <-ctx.Done():
			return nil
		case <-wake:
		case <-time.After(containersStatsInterval):
		}
	}
}

// readContainersStatsPump is the single reader for the ContainersStats websocket. It stores each
// subscription and wakes the sampler; malformed frames are ignored. Do not add other readers.
func readContainersStatsPump(ctx context.Context, cancel context.CancelFunc, conn *websocket.Conn, ids *atomic.Pointer[[]string], wake chan<- struct{}) {
	defer cancel()
	for {
		_, data, err := conn.Read(ctx)
		if err != nil {
			return
		}
		var subscription containertypes.ResourceSampleSubscription
		if json.Unmarshal(data, &subscription) != nil {
			continue
		}
		ids.Store(new(subscription.ContainerIDs))
		select {
		case wake <- struct{}{}:
		default:
		}
	}
}

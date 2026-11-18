package devices

import (
	"context"

	"github.com/nwverse/persona/server/internal/i18n"
	"github.com/nwverse/persona/server/internal/proto"
)

// The owner's messages to an Agent whose computer is away, held and sent once its node is back (#10661).
// Contract stubs: the implementation follows in this file.

// HeldMessages is assistant.Devices'.
func (d *Devices) HeldMessages(machine string, lang i18n.Lang) map[string][]proto.UndeliveredMessage {
	return nil
}

// HeldRetry is assistant.Devices'.
func (d *Devices) HeldRetry(ctx context.Context, sid, msg string) (bool, error) { return false, nil }

// HeldDelete is assistant.Devices'.
func (d *Devices) HeldDelete(ctx context.Context, sid, msg string) (bool, error) { return false, nil }

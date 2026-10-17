// SPDX-License-Identifier: AGPL-3.0-or-later

package worker_test

import (
	"io"
	"log/slog"
)

// quietTestLogger keeps worker logs out of test output.
func quietTestLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

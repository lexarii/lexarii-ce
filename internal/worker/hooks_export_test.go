// SPDX-License-Identifier: AGPL-3.0-or-later

package worker

// SetAfterParse installs a hook that runs between parsing a file and storing
// it, for tests that drive one exact interleaving. Compiled into tests only.
func SetAfterParse(i *Ingest, f func()) { i.afterParse = f }

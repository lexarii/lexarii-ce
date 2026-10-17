// SPDX-License-Identifier: AGPL-3.0-or-later

import type { User } from '$api/client';

/**
 * Whether to offer asking the library at all.
 *
 * Only when the server says a chat model is configured (`canAsk` on
 * /auth/me). Without one every question fails, so the entry points are hidden
 * rather than leading to a box that cannot answer. An absent field - an older
 * server, or a failed check - counts as no: a hidden link costs less than a
 * page of errors, and the server refuses either way.
 */
export function offersAsk(me: Pick<User, 'canAsk'> | null | undefined): boolean {
	return me?.canAsk === true;
}

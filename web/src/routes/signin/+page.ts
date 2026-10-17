// SPDX-License-Identifier: AGPL-3.0-or-later
// The sign-in page is the one every signed-out visitor is sent to, so it is
// public by definition.
import { PUBLIC_ROUTE } from '$lib/auth/public';

export const load = () => PUBLIC_ROUTE;

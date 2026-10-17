// SPDX-License-Identifier: AGPL-3.0-or-later
import * as api from '$api/client';
import { brandingProbe } from '$lib/branding';

// Static adapter: the Go binary serves the built client, so there is no
// server-side rendering at runtime.
export const ssr = false;
export const prerender = false;

const branding = brandingProbe(() => api.authOptions());

/**
 * Whether to load the installation's stylesheet, as a promise the layout
 * waits on by itself: how the app looks must never hold up what it shows.
 */
export const load = () => ({ branding: branding() });

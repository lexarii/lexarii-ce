<!-- SPDX-License-Identifier: AGPL-3.0-or-later -->
<script lang="ts">
	import { goto } from '$app/navigation';
	import { page } from '$app/state';
	import '$lib/styles/tokens.css';
	import '$lib/styles/base.css';
	import * as api from '$api/client';
	import { ApiError } from '$api/client';
	import { isPublicRoute } from '$lib/auth/public';
	import { BRANDING_STYLESHEET, pageName } from '$lib/branding';
	import type { LayoutProps } from './$types';

	let { children, data }: LayoutProps = $props();

	/**
	 * Which page this is, on <body> as data-page, so an installation's
	 * branding stylesheet can style one page without touching the others.
	 * On <body> rather than a wrapper because a background belongs there.
	 * See docs/BRANDING.md.
	 */
	const currentPage = $derived(pageName(page.route.id));
	$effect(() => {
		document.body.dataset.page = currentPage;
		return () => {
			delete document.body.dataset.page;
		};
	});

	let checked = $state(false);
	let signedIn = $state(false);

	/**
	 * A route shown without signing in - /signin, and any route whose load
	 * returns PUBLIC_ROUTE. See $lib/auth/public.
	 */
	const isPublic = $derived(isPublicRoute(page.data));

	/**
	 * The client-side guard.
	 *
	 * This is a convenience, not the enforcement. Every protected endpoint
	 * rejects an unauthenticated request on the server, and it has to: anyone
	 * can skip a client-side check. What this does is send a signed-out person
	 * to the sign-in page instead of showing them a shelf full of errors.
	 */
	$effect(() => {
		if (isPublic) {
			checked = true;
			return;
		}

		api
			.me()
			.then(() => {
				signedIn = true;
				checked = true;
			})
			.catch((err) => {
				checked = true;
				if (err instanceof ApiError && err.status === 401) {
					void goto('/signin', { replaceState: true });
				}
			});
	});
</script>

<svelte:head>
	<!--
		The installation's own stylesheet, when it has one. Appended once the
		server has answered, so it comes after the design tokens and base
		styles and overrides them on a tie (the branding hooks' own defaults
		carry no specificity, so it wins those in any order). Never waited on:
		how the app looks must not hold up what it shows.
	-->
	{#await data.branding then branded}
		{#if branded}
			<link rel="stylesheet" href={BRANDING_STYLESHEET} />
		{/if}
	{/await}
</svelte:head>

{#if isPublic || signedIn}
	{@render children()}
{:else if checked}
	<!-- The redirect is in flight. -->
	<p class="waiting">Signing in…</p>
{/if}

<style>
	.waiting {
		margin: 12vh auto;
		text-align: center;
		color: var(--fg-muted);
		font-size: var(--text-sm);
	}
</style>

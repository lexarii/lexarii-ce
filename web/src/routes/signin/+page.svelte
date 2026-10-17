<!-- SPDX-License-Identifier: AGPL-3.0-or-later -->
<script lang="ts">
	import { goto } from '$app/navigation';
	import { page } from '$app/state';
	import * as api from '$api/client';
	import type { AuthOptions } from '$api/client';
	import { isExternalHref, safeSignInHref } from '$lib/auth/public';

	let options = $state<AuthOptions | null>(null);
	let username = $state('');
	let password = $state('');
	let confirm = $state('');
	let email = $state('');
	let busy = $state(false);
	let error = $state<string | null>(null);

	const setupMode = $derived(options?.setupRequired === true);
	const passwordProviders = $derived(options?.providers.filter((p) => p.kind === 'password') ?? []);
	const redirectProviders = $derived(options?.providers.filter((p) => p.kind === 'redirect') ?? []);
	// The administrator's own links. Checked again here although the server
	// refuses a bad one at startup: this page is shown to strangers.
	const links = $derived(
		(options?.links ?? []).flatMap((link) => {
			const href = safeSignInHref(link.href);
			return href ? [{ label: link.label, href, external: isExternalHref(href) }] : [];
		})
	);

	$effect(() => {
		// A failed federated sign-in redirects here with a message rather than
		// returning JSON to what was a full-page navigation.
		const redirected = page.url.searchParams.get('error');
		if (redirected) error = redirected;

		api
			.authOptions()
			.then((o) => (options = o))
			.catch((err) => (error = err instanceof Error ? err.message : String(err)));
	});

	async function submit(event: SubmitEvent) {
		event.preventDefault();
		error = null;

		if (setupMode && password !== confirm) {
			error = 'The passwords do not match.';
			return;
		}

		busy = true;
		try {
			if (setupMode) {
				await api.setup(username, password, email || undefined);
			} else {
				await api.login(username, password);
			}
			await goto('/', { replaceState: true, invalidateAll: true });
		} catch (err) {
			error = err instanceof Error ? err.message : String(err);
		} finally {
			busy = false;
		}
	}
</script>

<svelte:head><title>{setupMode ? 'Set up' : 'Sign in'} · Lexarii</title></svelte:head>

<!--
	The class names and data-page here are a supported contract for an
	installation's branding stylesheet (docs/BRANDING.md): renaming one breaks
	somebody's sign-in page. Their default styles are written in :where() so
	they carry no specificity, and any rule a stylesheet writes for them wins.
-->
<main data-page="signin">
	<!-- Nothing by default; a branding stylesheet may give it a background. -->
	<div class="auth-decoration" aria-hidden="true"></div>

	<div class="auth-card">
		<h1 class="auth-brand">Lexarii</h1>

		{#if !options}
			<p class="muted">Loading…</p>
		{:else}
			{#if setupMode}
				<p class="lead">
					No accounts yet. Create the first one — it will be the administrator.
				</p>
			{/if}

			<form onsubmit={submit}>
				<label>
					<span>Username</span>
					<input bind:value={username} autocomplete="username" required />
				</label>

				{#if setupMode}
					<label>
						<span>Email <em>(optional)</em></span>
						<input type="email" bind:value={email} autocomplete="email" />
					</label>
				{/if}

				<label>
					<span>Password</span>
					<input
						type="password"
						bind:value={password}
						autocomplete={setupMode ? 'new-password' : 'current-password'}
						required
					/>
				</label>

				{#if setupMode}
					<label>
						<span>Confirm password</span>
						<input type="password" bind:value={confirm} autocomplete="new-password" required />
					</label>
					<!--
						Length only. Composition rules reliably produce "Password1!" and
						nothing safer.
					-->
					<p class="hint">At least 10 characters. A memorable phrase beats a short, complicated word.</p>
				{/if}

				{#if error}
					<p class="error" role="alert">{error}</p>
				{/if}

				<button type="submit" disabled={busy}>
					{busy ? 'Working…' : setupMode ? 'Create account' : 'Sign in'}
				</button>
			</form>

			{#if redirectProviders.length > 0}
				<!--
					Federated providers render as buttons rather than a form. The login
					screen asks the server what exists, so adding OIDC or LDAP changes
					this page without changing the client.
				-->
				<div class="alternatives">
					<span class="divider">or</span>
					{#each redirectProviders as provider (provider.name)}
						<a class="alt" href="/api/v1/auth/{provider.name}/start">
							Continue with {provider.displayName}
						</a>
					{/each}
				</div>
			{/if}

			{#if passwordProviders.length > 1}
				<p class="muted">
					Signing in with {passwordProviders[0]?.displayName}.
				</p>
			{/if}

			{#if links.length > 0}
				<!--
					Full navigations, not client-side ones: a path here may be served by
					something in front of Lexarii rather than by this app.
				-->
				<nav class="auth-links" aria-label="Help with signing in">
					<ul>
						{#each links as link, i (i)}
							<li>
								<a
									href={link.href}
									data-sveltekit-reload
									rel={link.external ? 'external noopener noreferrer' : undefined}
								>
									{link.label}
								</a>
							</li>
						{/each}
					</ul>
				</nav>
			{/if}
		{/if}
	</div>
</main>

<style>
	/* The branding hooks: :where() keeps each at zero specificity. */
	:where(.auth-decoration) {
		position: fixed;
		inset: 0;
		z-index: -1;
		pointer-events: none;
	}

	:where(.auth-card) {
		max-width: 22rem;
		margin: 12vh auto;
		padding: 0 var(--space-4);
	}

	:where(.auth-brand) {
		font-family: var(--font-serif);
		font-size: var(--text-2xl);
		margin: 0 0 var(--space-2);
	}

	.lead {
		color: var(--fg-muted);
		margin: 0 0 var(--space-6);
	}

	form {
		display: flex;
		flex-direction: column;
		gap: var(--space-4);
		margin-top: var(--space-6);
	}

	label {
		display: flex;
		flex-direction: column;
		gap: var(--space-1);
		font-size: var(--text-sm);
		color: var(--fg-muted);
	}

	em {
		font-style: normal;
		color: var(--fg-subtle);
	}

	input {
		min-height: 44px;
		padding: 0 var(--space-3);
		border: 1px solid var(--border);
		border-radius: var(--radius-md);
		background: var(--bg-raised);
		color: var(--fg);
		font: inherit;
	}

	button {
		min-height: 44px;
		border: 0;
		border-radius: var(--radius-md);
		background: var(--accent);
		color: var(--accent-fg);
		font: inherit;
		font-size: var(--text-sm);
		cursor: pointer;
	}

	button[disabled] {
		opacity: 0.6;
		cursor: default;
	}

	.hint,
	.muted {
		font-size: var(--text-xs);
		color: var(--fg-subtle);
		margin: 0;
	}

	.error {
		color: var(--danger);
		font-size: var(--text-sm);
		margin: 0;
	}

	.alternatives {
		display: flex;
		flex-direction: column;
		gap: var(--space-3);
		margin-top: var(--space-6);
	}

	.divider {
		text-align: center;
		color: var(--fg-subtle);
		font-size: var(--text-xs);
	}

	:where(.auth-links) {
		margin-top: var(--space-6);
		border-top: 1px solid var(--border);
		padding-top: var(--space-3);
	}

	.auth-links ul {
		display: flex;
		flex-wrap: wrap;
		justify-content: center;
		gap: 0 var(--space-4);
		margin: 0;
		padding: 0;
		list-style: none;
	}

	.auth-links a {
		display: inline-flex;
		align-items: center;
		min-height: 44px;
		color: var(--fg-muted);
		font-size: var(--text-sm);
		text-underline-offset: 0.2em;
		overflow-wrap: anywhere;
	}

	.auth-links a:hover {
		color: var(--fg);
	}

	.alt {
		display: inline-flex;
		align-items: center;
		justify-content: center;
		min-height: 44px;
		border: 1px solid var(--border);
		border-radius: var(--radius-md);
		color: var(--fg);
		text-decoration: none;
		font-size: var(--text-sm);
	}
</style>

<!-- SPDX-License-Identifier: AGPL-3.0-or-later -->
<script lang="ts">
	import { goto } from '$app/navigation';
	import * as api from '$api/client';
	import { applyTheme, savedTheme, themeLabels, themes, type Theme } from '$lib/theme';
	import type { ArchivedBook, Person, Session, User } from '$api/client';

	let user = $state<User | null>(null);

	// Read once, from the same place the inline script in app.html reads, so
	// the control agrees with what is already on screen.
	let theme = $state<Theme>('system');
	$effect(() => {
		theme = savedTheme();
	});
	let sessions = $state<Session[]>([]);
	let loading = $state(true);
	let error = $state<string | null>(null);
	let busy = $state<string | null>(null);
	let confirmingAll = $state(false);

	// Deleting your own account. Typing the name rather than clicking twice:
	// this destroys every note and position the person has, and no amount of
	// confirmation dialogue makes that undoable.
	let deleteConfirmation = $state('');
	let deleting = $state(false);

	// The administrator's half: other accounts, and the books left behind by
	// accounts that are already gone.
	let people = $state<Person[]>([]);
	let archived = $state<ArchivedBook[]>([]);
	let confirmingDelete = $state<string | null>(null);
	let adminNote = $state<string | null>(null);

	$effect(() => {
		void load();
	});

	async function load() {
		loading = true;
		try {
			const [who, list] = await Promise.all([api.me(), api.sessions()]);
			user = who;
			sessions = list.items;
			error = null;
			if (who.isAdmin) void loadAdmin();
		} catch (err) {
			error = err instanceof Error ? err.message : String(err);
		} finally {
			loading = false;
		}
	}

	async function loadAdmin() {
		try {
			const [p, a] = await Promise.all([api.people(), api.archivedBooks()]);
			people = p.items;
			archived = a.items;
		} catch {
			/* not an administrator any more, or the call failed: show nothing */
			people = [];
			archived = [];
		}
	}

	/**
	 * Deletes this person's own account and signs them out.
	 *
	 * There is no undo and nothing to come back to: the books survive, but they
	 * survive as somebody else's problem to hand back.
	 */
	async function deleteSelf() {
		if (!user || deleteConfirmation !== user.username) return;
		deleting = true;
		try {
			await api.deleteAccount(user.id);
			await goto('/signin');
		} catch (err) {
			error = err instanceof Error ? err.message : String(err);
			deleting = false;
		}
	}

	async function deletePerson(person: Person) {
		busy = person.id;
		adminNote = null;
		try {
			const result = await api.deleteAccount(person.id);
			confirmingDelete = null;
			adminNote =
				result.booksArchived === 0
					? `${person.username} is gone. They owned no books.`
					: `${person.username} is gone. ${result.booksArchived} book${
							result.booksArchived === 1 ? '' : 's'
						} archived, ${result.filesMoved} file${result.filesMoved === 1 ? '' : 's'} moved.`;
			await loadAdmin();
		} catch (err) {
			adminNote = err instanceof Error ? err.message : String(err);
		} finally {
			busy = null;
		}
	}

	// Who each archived book should go to. Empty means the administrator
	// doing the claiming, which is the common case and stays one click.
	let giveTo = $state<Record<string, string>>({});

	async function claim(book: ArchivedBook) {
		busy = book.bookFileId;
		adminNote = null;
		const ownerId = giveTo[book.bookFileId] || undefined;
		const recipient = people.find((p) => p.id === ownerId);
		try {
			await api.claimArchived(book.bookFileId, ownerId);
			adminNote = recipient
				? `“${book.title}” is ${recipient.username}'s now.`
				: `“${book.title}” is yours now.`;
			await loadAdmin();
		} catch (err) {
			adminNote = err instanceof Error ? err.message : String(err);
		} finally {
			busy = null;
		}
	}

	async function revoke(session: Session) {
		busy = session.id;
		try {
			await api.revokeSession(session.id);
			// Ending the session you are using signs you out here too, so there is
			// nothing left to show.
			if (session.current) {
				await goto('/signin');
				return;
			}
			sessions = sessions.filter((s) => s.id !== session.id);
		} catch (err) {
			error = err instanceof Error ? err.message : String(err);
		} finally {
			busy = null;
		}
	}

	async function revokeAll() {
		busy = 'all';
		try {
			await api.revokeAllSessions();
			await goto('/signin');
		} catch (err) {
			error = err instanceof Error ? err.message : String(err);
			busy = null;
		}
	}

	async function signOut() {
		try {
			await api.logout();
		} catch {
			/* the cookie is cleared either way */
		}
		await goto('/signin');
	}

	/**
	 * A readable name for a browser's own claim about itself.
	 *
	 * A user agent string is unverified and unreadable, and the point of this
	 * screen is recognising a device at a glance. Anything not recognised is
	 * shown as it came rather than guessed at.
	 */
	function deviceName(agent: string | undefined): string {
		if (!agent) return 'Unknown device';

		const browser =
			/Firefox\/[\d.]+/.test(agent) && !/Seamonkey/.test(agent)
				? 'Firefox'
				: /Edg\//.test(agent)
					? 'Edge'
					: /OPR\//.test(agent)
						? 'Opera'
						: /Chrome\//.test(agent) && !/Chromium/.test(agent)
							? 'Chrome'
							: /Chromium\//.test(agent)
								? 'Chromium'
								: /Safari\//.test(agent) && !/Chrome/.test(agent)
									? 'Safari'
									: null;

		const platform = /Android/.test(agent)
			? 'Android'
			: /iPhone|iPad|iPod/.test(agent)
				? 'iOS'
				: /Windows/.test(agent)
					? 'Windows'
					: /Mac OS X|Macintosh/.test(agent)
						? 'macOS'
						: /Linux/.test(agent)
							? 'Linux'
							: null;

		if (browser && platform) return `${browser} on ${platform}`;
		if (browser) return browser;
		if (platform) return platform;
		return agent.length > 60 ? agent.slice(0, 60) + '…' : agent;
	}

	function when(iso: string): string {
		const then = new Date(iso);
		const seconds = Math.round((Date.now() - then.getTime()) / 1000);
		if (seconds < 90) return 'just now';
		const minutes = Math.round(seconds / 60);
		if (minutes < 60) return `${minutes} minutes ago`;
		const hours = Math.round(minutes / 60);
		if (hours < 24) return `${hours} ${hours === 1 ? 'hour' : 'hours'} ago`;
		const days = Math.round(hours / 24);
		if (days < 30) return `${days} ${days === 1 ? 'day' : 'days'} ago`;
		return then.toLocaleDateString();
	}

	function providerLabel(name: string | undefined): string {
		switch (name) {
			case 'local':
				return 'Password';
			case 'oidc':
				return 'Single sign-on';
			case 'ldap':
				return 'Directory';
			case 'ad':
				return 'Active Directory';
			case 'proxy':
				return 'Proxy';
			default:
				return name ?? '';
		}
	}
</script>

<svelte:head><title>Account · Lexarii</title></svelte:head>

<div class="shell">
	<header>
		<a class="back" href="/">← Library</a>
		<h1>Account</h1>
		{#if user}
			<p class="who">
				Signed in as <strong>{user.username}</strong>
				{#if user.isAdmin}<span class="tag">administrator</span>{/if}
			</p>
		{/if}
	</header>

	{#if error}
		<p class="error" role="alert">{error}</p>
	{/if}

	<!--
		The library's appearance, not the reader's. The reader keeps its own
		palette and its own switch: somebody reading in sepia does not
		necessarily want a sepia library behind it.
	-->
	<section>
		<h2>Appearance</h2>
		<p class="hint">
			How the library, the chat and these settings look. The reader has its own
			setting, because a page of prose and an application are different things.
		</p>

		<div class="themes" role="radiogroup" aria-label="Theme">
			{#each themes as option (option)}
				<label class="theme" class:chosen={theme === option}>
					<input
						type="radio"
						name="theme"
						value={option}
						checked={theme === option}
						onchange={() => {
							theme = option;
							applyTheme(option);
						}}
					/>
					{themeLabels[option]}
				</label>
			{/each}
		</div>
	</section>

	<section>
		<h2>Signed-in devices</h2>
		<p class="hint">
			Everything that can reach your library right now. Ending a session signs that
			device out immediately.
		</p>

		{#if loading}
			<p class="loading">Loading…</p>
		{:else if sessions.length === 0}
			<p class="empty">No other sessions.</p>
		{:else}
			<ul class="sessions">
				{#each sessions as session (session.id)}
					<li class:current={session.current}>
						<div class="detail">
							<!--
								A session with a device name is a bearer token held by an
								app, not a cookie held by a browser. The name is the one
								that person typed when they signed the app in, which is
								the only thing that tells two identical Android
								user-agents apart a year later.
							-->
							<span class="device">
								{session.device || deviceName(session.userAgent)}
								{#if session.current}<span class="tag">this device</span>{/if}
								{#if session.device}<span class="tag app">app</span>{/if}
							</span>
							<span class="meta">
								{#if session.clientIp}{session.clientIp} ·{/if}
								last used {when(session.lastUsedAt)}
								{#if session.provider}· {providerLabel(session.provider)}{/if}
							</span>
						</div>
						<button
							class="button danger"
							onclick={() => void revoke(session)}
							disabled={busy === session.id}
						>
							{busy === session.id ? 'Ending…' : session.current ? 'Sign out' : 'End'}
						</button>
					</li>
				{/each}
			</ul>
		{/if}
	</section>

	<section>
		<h2>If something looks wrong</h2>
		<p class="hint">
			Ending every session signs out every device, including this one. Do this if you
			think someone else has your session.
		</p>
		{#if confirmingAll}
			<div class="confirm">
				<span>End every session and sign out here?</span>
				<button class="button danger" onclick={() => void revokeAll()} disabled={busy === 'all'}>
					{busy === 'all' ? 'Ending…' : 'Yes, end all'}
				</button>
				<button class="button" onclick={() => (confirmingAll = false)}>Cancel</button>
			</div>
		{:else}
			<div class="row">
				<button class="button danger" onclick={() => (confirmingAll = true)}>
					End all sessions
				</button>
				<button class="button" onclick={() => void signOut()}>Sign out</button>
			</div>
		{/if}
	</section>

	{#if user?.isAdmin && archived.length > 0}
		<section>
			<h2>Books left behind</h2>
			<p class="hint">
				These belonged to accounts that were deleted. They are on nobody's shelf
				until somebody takes them. Their records are deleted after six months;
				the files are kept either way.
			</p>
			<ul class="archived">
				{#each archived as book (book.bookFileId)}
					<li>
						<span class="what">{book.title}</span>
						<span class="who">left by {book.leftBy || 'a deleted account'}</span>
						<span class="when">{new Date(book.archivedAt).toLocaleDateString()}</span>
						<span class="where">
							{book.kept ? 'in the archive' : 'still in the library folder'}
						</span>
						<!--
							A book left behind is not always the administrator's to
							keep. The endpoint has always accepted a person; there
							was simply no way to name one, so tidying up after
							somebody meant taking their books yourself and sharing
							them back.
						-->
						{#if people.length > 0}
							<label class="give">
								<span class="sr-only">Give “{book.title}” to</span>
								<select bind:value={giveTo[book.bookFileId]} disabled={busy === book.bookFileId}>
									<option value="">me</option>
									{#each people as person (person.id)}
										<option value={person.id}>{person.username}</option>
									{/each}
								</select>
							</label>
						{/if}
						<button
							class="button"
							disabled={busy === book.bookFileId}
							onclick={() => void claim(book)}
						>
							{#if busy === book.bookFileId}
								Taking…
							{:else if giveTo[book.bookFileId]}
								Hand it over
							{:else}
								Take it
							{/if}
						</button>
					</li>
				{/each}
			</ul>
		</section>
	{/if}

	{#if user?.isAdmin}
		<section>
			<h2>Blocked files</h2>
			<p class="hint">
				Files removed from this library for everybody, from a book's page, and kept from
				coming back.
			</p>
			<a class="button" href="/blocked">See blocked files</a>
		</section>
	{/if}

	{#if user?.isAdmin && people.length > 0}
		<section>
			<h2>Other accounts</h2>
			<p class="hint">
				Deleting somebody removes them and everything of theirs: notes, reading
				positions, conversations. Books they added are archived under their name
				and appear above. Books lent TO them simply return to whoever owns them.
			</p>
			<ul class="people">
				{#each people as person (person.id)}
					<li>
						<span class="who">{person.username}</span>
						{#if person.isAdmin}<span class="tag">administrator</span>{/if}
						{#if confirmingDelete === person.id}
							<button
								class="button danger"
								disabled={busy === person.id}
								onclick={() => void deletePerson(person)}
							>
								{busy === person.id ? 'Deleting…' : `Yes, delete ${person.username}`}
							</button>
							<button class="button" onclick={() => (confirmingDelete = null)}>Cancel</button>
						{:else}
							<button class="button danger" onclick={() => (confirmingDelete = person.id)}>
								Delete account
							</button>
						{/if}
					</li>
				{/each}
			</ul>
		</section>
	{/if}

	{#if adminNote}
		<p class="note" role="status">{adminNote}</p>
	{/if}

	<section class="leaving">
		<h2>Delete this account</h2>
		<p class="hint">
			Your notes, highlights, reading positions and conversations are deleted
			immediately and cannot be recovered.
		</p>
		<p class="hint">
			Books you added are kept and archived under your name, so an administrator
			can take them on — and so they are still here if you come back. Books lent
			to you go back to whoever owns them.
		</p>
		{#if user}
			<label class="confirm-name">
				Type <strong>{user.username}</strong> to confirm
				<input
					bind:value={deleteConfirmation}
					autocomplete="off"
					aria-label="Type your username to confirm deletion"
				/>
			</label>
			<button
				class="button danger"
				disabled={deleteConfirmation !== user.username || deleting}
				onclick={() => void deleteSelf()}
			>
				{deleting ? 'Deleting…' : 'Delete my account'}
			</button>
		{/if}
	</section>
</div>

<style>
	/* The select needs a label and the row has no room for one: the book's
	   title is right there and repeating it would be noise on screen and
	   necessary in a screen reader. */
	.sr-only {
		position: absolute;
		width: 1px;
		height: 1px;
		margin: -1px;
		padding: 0;
		overflow: hidden;
		clip-path: inset(50%);
		white-space: nowrap;
	}

	.give select {
		min-height: 44px;
		padding: 0 var(--space-2);
		border: 1px solid var(--border);
		border-radius: var(--radius-md);
		background: var(--bg-raised);
		color: var(--fg);
		font: inherit;
		font-size: var(--text-sm);
	}

	.archived li,
	.people li {
		display: flex;
		flex-wrap: wrap;
		align-items: center;
		gap: var(--space-2) var(--space-3);
		padding: var(--space-2) 0;
		border-bottom: 1px solid var(--border);
	}

	.archived .what {
		font-weight: 600;
	}

	.archived .who,
	.archived .when,
	.archived .where {
		color: var(--fg-muted);
		font-size: var(--text-sm);
	}

	.archived button,
	.people button {
		margin-left: auto;
	}

	.note {
		margin: 0 0 var(--space-4);
		padding: var(--space-2) var(--space-3);
		border-left: 3px solid var(--accent);
		background: var(--bg-raised);
		font-size: var(--text-sm);
	}

	/* The last thing on the page, and visibly separate from the rest of it. */
	.leaving {
		border-top: 1px solid var(--border);
		padding-top: var(--space-6);
	}

	.confirm-name {
		display: flex;
		flex-wrap: wrap;
		align-items: center;
		gap: var(--space-2);
		margin-bottom: var(--space-3);
		font-size: var(--text-sm);
		color: var(--fg-muted);
	}

	.confirm-name input {
		min-height: 44px;
		padding: 0 var(--space-3);
		border: 1px solid var(--border);
		border-radius: var(--radius-md);
		background: var(--bg-raised);
		color: var(--fg);
		font: inherit;
	}

	.shell {
		max-width: 44rem;
		margin: 0 auto;
		padding: var(--space-6) var(--space-4) var(--space-12);
	}

	header {
		margin-bottom: var(--space-8);
	}

	.back {
		display: inline-block;
		margin-bottom: var(--space-4);
		color: var(--fg-muted);
		font-size: var(--text-sm);
		text-decoration: none;
	}

	.back:hover {
		color: var(--fg);
	}

	h1 {
		margin: 0;
		font-family: var(--font-serif);
		font-size: var(--text-2xl);
	}

	.who {
		margin: var(--space-2) 0 0;
		color: var(--fg-muted);
		font-size: var(--text-sm);
	}

	section {
		margin-bottom: var(--space-8);
	}

	h2 {
		margin: 0 0 var(--space-2);
		font-size: var(--text-sm);
		text-transform: uppercase;
		letter-spacing: 0.06em;
		color: var(--fg-muted);
	}

	.hint {
		margin: 0 0 var(--space-4);
		color: var(--fg-muted);
		font-size: var(--text-sm);
		max-width: var(--measure);
	}

	.themes {
		display: flex;
		flex-wrap: wrap;
		gap: var(--space-2);
	}

	.theme {
		display: inline-flex;
		align-items: center;
		gap: var(--space-2);
		/* A touch target, not a hit-the-dot exercise. */
		min-height: 44px;
		padding: 0 var(--space-3);
		border: 1px solid var(--border);
		border-radius: var(--radius-md);
		font-size: var(--text-sm);
		cursor: pointer;
	}

	.theme.chosen {
		border-color: var(--accent);
		color: var(--accent);
	}

	.sessions {
		list-style: none;
		margin: 0;
		padding: 0;
		border: 1px solid var(--border);
		border-radius: var(--radius-md);
		overflow: hidden;
	}

	.sessions li {
		display: flex;
		align-items: center;
		gap: var(--space-4);
		padding: var(--space-3) var(--space-4);
		background: var(--bg-raised);
	}

	.sessions li + li {
		border-top: 1px solid var(--border);
	}

	.sessions li.current {
		background: var(--bg-sunken);
	}

	.detail {
		display: flex;
		flex-direction: column;
		gap: var(--space-1);
		min-width: 0;
		flex: 1;
	}

	.device {
		display: flex;
		align-items: center;
		gap: var(--space-2);
		font-size: var(--text-base);
	}

	.meta {
		color: var(--fg-muted);
		font-size: var(--text-sm);
		overflow-wrap: anywhere;
	}

	.tag {
		padding: 0 var(--space-2);
		border-radius: var(--radius-sm);
		background: var(--accent);
		color: var(--accent-fg);
		font-size: var(--text-xs);
		text-transform: uppercase;
		letter-spacing: 0.04em;
	}

	.tag.app {
		/* Said plainly rather than implied: this one is a token an app holds,
		   which is a different thing from a cookie in a browser. */
		background: var(--bg-sunken);
		color: var(--fg-muted);
	}

	.row,
	.confirm {
		display: flex;
		flex-wrap: wrap;
		align-items: center;
		gap: var(--space-3);
		font-size: var(--text-sm);
	}

	.button {
		display: inline-flex;
		align-items: center;
		min-height: 44px;
		padding: 0 var(--space-4);
		border: 1px solid var(--border);
		border-radius: var(--radius-md);
		background: var(--bg-raised);
		color: var(--fg);
		font-size: var(--text-sm);
		text-decoration: none;
		cursor: pointer;
	}

	.button:hover:not(:disabled) {
		background: var(--bg-sunken);
	}

	.button:disabled {
		opacity: 0.6;
		cursor: default;
	}

	.button.danger {
		color: var(--danger);
		border-color: color-mix(in srgb, var(--danger) 40%, var(--border));
	}

	.error {
		margin: 0 0 var(--space-4);
		color: var(--danger);
		font-size: var(--text-sm);
	}

	.loading,
	.empty {
		color: var(--fg-muted);
		font-size: var(--text-sm);
	}

	@media (max-width: 30rem) {
		.sessions li {
			flex-direction: column;
			align-items: stretch;
		}

		.sessions li .button {
			justify-content: center;
		}
	}
</style>

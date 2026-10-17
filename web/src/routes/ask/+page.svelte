<!-- SPDX-License-Identifier: AGPL-3.0-or-later -->
<script lang="ts">
	import { tick } from 'svelte';
	import { page } from '$app/state';
	import { goto } from '$app/navigation';
	import * as api from '$api/client';
	import { askFailure, blocks, segments, withReadingList, type AskFailure } from '$lib/answer';
	import { offersAsk } from '$lib/auth/capabilities';
	import type { ChatMessage, ConversationSummary, Source } from '$api/client';

	let question = $state('');
	let messages = $state<ChatMessage[]>([]);
	let threads = $state<ConversationSummary[]>([]);
	let conversationId = $state<string | null>(null);

	/**
	 * The book this thread is about, when it is about one.
	 *
	 * `?book=<bookFileId>` arrives from the reader. Retrieval has always been
	 * able to answer from a single book - the scope reaches the search, and the
	 * server pins a thread to it for good once the first question lands - and
	 * nothing in the client ever set it, so the whole library was searched even
	 * when somebody was plainly asking about the page in front of them.
	 *
	 * The title is fetched rather than passed in the URL: it has to be right,
	 * and the only thing that knows it is the book.
	 */
	let pinnedBook = $state<{ id: string; title: string } | null>(null);

	function pinTo(id: string | null) {
		if (!id) {
			pinnedBook = null;
			return;
		}
		if (pinnedBook?.id === id) return;
		// Shown as soon as it is known, named as soon as the name arrives: a
		// pin nobody can see is a pin nobody asked for.
		pinnedBook = { id, title: '' };
		void api
			.bookInfo(id)
			.then((info) => {
				if (pinnedBook?.id === id) pinnedBook = { id, title: info.title };
			})
			.catch(() => {
				// A book that cannot be named is still a book that can be asked
				// about: the scope is the id, not the title.
			});
	}

	$effect(() => {
		pinTo(page.url.searchParams.get('book'));
	});

	let asking = $state(false);
	// Which turn is being written, so it can show a cursor and nothing else has
	// to guess whether the text is finished.
	let streamingIndex = $state<number | null>(null);
	let error = $state<AskFailure | null>(null);
	let showThreads = $state(false);

	// The passage list is collapsed by default. It is the evidence, and it
	// should be one click away rather than in the way of reading the answer.
	let openSources = $state<Record<string, boolean>>({});

	let transcript = $state<HTMLElement | null>(null);

	$effect(() => {
		void loadThreads();
	});

	/**
	 * Whether this server can answer at all. Null until known, so the page
	 * does not flash a "no model" notice at somebody who has one. The library
	 * hides its Ask link without a model; this is for anybody who arrives
	 * here anyway - a bookmark, or the reader's own link.
	 */
	let canAsk = $state<boolean | null>(null);

	$effect(() => {
		api
			.me()
			.then((me) => (canAsk = offersAsk(me)))
			.catch(() => {
				// Unknown: leave the page as it was rather than claim no model.
				canAsk = true;
			});
	});

	async function loadThreads() {
		try {
			threads = (await api.conversations()).items;
		} catch {
			/* the list is a convenience; asking still works without it */
		}
	}

	async function openThread(id: string) {
		showThreads = false;
		try {
			const thread = await api.conversation(id);
			conversationId = thread.id;
			// A turn that answered with a reading list reads back as one,
			// written exactly as it was when it arrived.
			messages = thread.messages.map((m) =>
				m.books?.length ? { ...m, content: withReadingList(m.content, m.books) } : m
			);
			error = null;
			// A thread carries its own scope: the server pinned it when it was
			// started and every later question inherits that, so the chip has to
			// follow the thread rather than whatever the URL last said.
			pinTo(thread.bookFileId ?? null);
			await scrollToEnd();
		} catch (err) {
			error = askFailure(err);
		}
	}

	function newThread() {
		conversationId = null;
		messages = [];
		error = null;
		showThreads = false;
	}

	/**
	 * Leaves the book and asks the whole library again.
	 *
	 * A thread that has already been asked something keeps the scope it was
	 * created with - that is the server's rule, and it is the right one, since a
	 * thread whose scope moved halfway through would answer its own history
	 * inconsistently. So dropping the pin on a thread that has started means
	 * starting a fresh one; otherwise the chip would say "your library" while
	 * the search stayed inside one book.
	 */
	async function askWholeLibrary() {
		pinnedBook = null;
		if (messages.length > 0) newThread();
		// The address stops saying "book" too, so a reload or a shared link does
		// not quietly pin it again.
		await goto('/ask', { replaceState: true, keepFocus: true, noScroll: true });
	}

	async function send() {
		const text = question.trim();
		if (!text || asking) return;

		asking = true;
		error = null;
		question = '';

		// The question appears immediately, with a placeholder answer beneath
		// it. A model on a machine in the next room takes twenty seconds, and a
		// box that does nothing for twenty seconds looks broken.
		const pending: ChatMessage = {
			id: 'pending-' + Date.now(),
			seq: messages.length,
			role: 'user',
			content: text,
			citations: [],
			createdAt: new Date().toISOString()
		};
		messages = [...messages, pending];
		await scrollToEnd();

		// The answer is built in place as it arrives. An empty assistant turn
		// goes in first so the text has somewhere to land, and so the reader
		// sees the answer being written rather than a spinner that ends.
		const answerIndex = messages.length;
		messages = [
			...messages,
			{
				id: 'streaming-' + Date.now(),
				seq: answerIndex,
				role: 'assistant',
				content: '',
				citations: [],
				createdAt: new Date().toISOString()
			}
		];
		streamingIndex = answerIndex;

		// Which thread this answer belongs to, captured before the wait.
		//
		// A person can open another thread while the model is still writing -
		// nothing disables the sidebar - and the completion path assigned into
		// messages[answerIndex] unconditionally, so twenty seconds later the
		// answer to the old question silently overwrote a message in the thread
		// now on screen. The streaming callbacks already guarded; the one that
		// replaces the whole turn did not.
		const askedIn = conversationId;
		const stillHere = () => conversationId === askedIn || askedIn === null;

		try {
			const answer = await api.askStreaming(
				text,
				{
					onConversation: (summary) => {
						// Recorded before the answer exists, so a reload during
						// a thirty-second wait finds the thread.
						conversationId = summary.id;
					},
					onPassages: (sources) => {
						// The evidence arrives seconds before the prose. Showing
						// it early is the difference between waiting and reading.
						if (!stillHere()) return;
						const turn = messages[answerIndex];
						if (turn) turn.citations = sources;
					},
					onDelta: (delta) => {
						if (!stillHere()) return;
						const turn = messages[answerIndex];
						if (turn) turn.content += delta;
						void scrollToEnd();
					}
				},
				conversationId ?? undefined,
				pinnedBook?.id
			);
			// The reader has moved to another thread: this answer belongs to the
			// one they left, which is already stored on the server and will be
			// there when they go back to it.
			if (!stillHere()) return;
			conversationId = answer.conversationId ?? conversationId;

			// The stored answer replaces what was streamed: same text, but with
			// the id, timings and the citations actually marked as cited.
			messages[answerIndex] = {
				id: answer.messageId ?? 'answer-' + Date.now(),
				seq: answerIndex,
				role: 'assistant',
				content: answer.answer,
				intent: answer.intent,
				model: answer.model,
				retrievalMs: answer.retrievalMs,
				generationMs: answer.generationMs,
				scopedTo: answer.scopedTo,
				citations: answer.sources,
				createdAt: new Date().toISOString()
			};

			// A reading list is an answer too, and it arrives as books rather
			// than prose.
			const latest = messages[answerIndex];
			if (latest && answer.books.length > 0) {
				latest.content = withReadingList(answer.answer, answer.books);
			}

			void loadThreads();
			await scrollToEnd();
		} catch (err) {
			error = askFailure(err);
			// A failed answer leaves the question in the thread - the person
			// asked it - but not an empty box pretending to be a reply.
			const turn = messages[answerIndex];
			if (turn && !turn.content) {
				messages = messages.filter((_, i) => i !== answerIndex);
			}
		} finally {
			asking = false;
			streamingIndex = null;
		}
	}

	async function scrollToEnd() {
		await tick();
		transcript?.scrollTo({ top: transcript.scrollHeight, behavior: 'smooth' });
	}

	function onKey(event: KeyboardEvent) {
		// Enter sends, shift+Enter is a new line: a question is usually one line
		// and reaching for a button every time is friction.
		if (event.key === 'Enter' && !event.shiftKey) {
			event.preventDefault();
			void send();
		}
	}

	function sourceFor(message: ChatMessage, number: number): Source | undefined {
		return message.citations.find((s) => s.number === number);
	}

	/** Where a passage opens: the PDF reader or the EPUB one. */
	/**
	 * Where a citation goes.
	 *
	 * To the PASSAGE, not to the top of the book. The locator has always been
	 * on the citation and the link threw it away, which made a citation a
	 * gesture at a book rather than a reference: following it left you at page
	 * one of eight hundred with the quoted sentence somewhere inside.
	 *
	 * Carried in the query string rather than the hash because the reader reads
	 * it once on load and a hash would be mistaken for an anchor.
	 */
	function readerHref(source: Source): string {
		const pdf = source.locator.startsWith('pdf:');
		const at = source.locator ? `?at=${encodeURIComponent(source.locator)}` : '';
		return `/read/${source.bookFileId}${pdf ? '/pdf' : ''}${at}`;
	}

	function timing(message: ChatMessage): string {
		const parts: string[] = [];
		// The question named a book and only that book was read. Said
		// first, because it is what explains an answer that ignores the
		// rest of the library.
		if (message.scopedTo) parts.push(`searched only ${message.scopedTo.title}`);
		if (message.retrievalMs) parts.push(`found in ${message.retrievalMs} ms`);
		if (message.generationMs) parts.push(`answered in ${(message.generationMs / 1000).toFixed(1)} s`);
		if (message.model) parts.push(message.model);
		return parts.join(' · ');
	}
</script>

<svelte:head><title>Ask · Lexarii</title></svelte:head>

<div class="shell">
	<header>
		<a class="back" href="/">← Library</a>
		<h1>Ask your library</h1>
		<div class="row">
			<button class="button" onclick={newThread} disabled={messages.length === 0}>New question</button>
			<button class="button" onclick={() => (showThreads = !showThreads)}>
				{showThreads ? 'Hide' : 'Earlier questions'}
			</button>
		</div>
	</header>

	{#if showThreads}
		<aside class="threads">
			{#if threads.length === 0}
				<p class="empty">Nothing yet.</p>
			{:else}
				<ul>
					{#each threads as thread (thread.id)}
						<li>
							<button class:current={thread.id === conversationId} onclick={() => void openThread(thread.id)}>
								{thread.title || 'Untitled question'}
							</button>
						</li>
					{/each}
				</ul>
			{/if}
		</aside>
	{/if}

	<div class="transcript" bind:this={transcript}>
		{#if canAsk === false && messages.length === 0}
			<div class="intro">
				<p>Asking needs a language model, and none is configured on this server.</p>
				<p class="hint">
					The library and the reader work without one. An administrator can set one up;
					earlier questions stay readable.
				</p>
			</div>
		{:else if messages.length === 0}
			<div class="intro">
				<p>Ask a question and it is answered from your own books, with the passages it used.</p>
				<p class="hint">
					Every claim carries a citation you can open in the reader. If your library does not
					cover something, it says so rather than inventing an answer.
				</p>
			</div>
		{/if}

		{#snippet inline(message: ChatMessage, text: string)}{#each segments(text) as part, n (n)}{#if part.cite !== null && sourceFor(message, part.cite)}{@const source = sourceFor(message, part.cite)!}<a
			class="cite"
			href={readerHref(source)}
			title="{source.label || source.title}">{part.text}</a>{:else if part.bold}<strong>{#if part.italic}<em>{part.text}</em>{:else}{part.text}{/if}</strong>{:else if part.code}<code>{part.text}</code>{:else if part.italic}<em>{part.text}</em>{:else}{part.text}{/if}{/each}{/snippet}

	{#each messages as message, i (message.id)}
			{#if message.role === 'user'}
				<div class="turn question">
					<p>{message.content}</p>
				</div>
			{:else}
				{@const body = blocks(message.content)}
				<div class="turn answer">
					<div class="body">
						{#each body as block, b (b)}
							{#if block.kind === 'ul'}
								<ul>
									{#each block.items as item, l (l)}
										<li>{@render inline(message, item)}</li>
									{/each}
								</ul>
							{:else}
								<p>{@render inline(message, block.text)}{#if i === streamingIndex &&
										b === body.length - 1}<span class="cursor" aria-hidden="true"></span>{/if}</p>
							{/if}
						{/each}
					</div>

					{#if message.citations.length > 0}
						<button
							class="sources-toggle"
							onclick={() => (openSources[message.id] = !openSources[message.id])}
						>
							{openSources[message.id] ? 'Hide' : 'Show'}
							{message.citations.length} passage{message.citations.length === 1 ? '' : 's'}
						</button>

						{#if openSources[message.id]}
							<ol class="sources">
								{#each message.citations as source (source.number)}
									<li class:uncited={!source.cited} class:mine={source.kind === 'note'}>
										<div class="source-head">
											<a href={readerHref(source)}>{source.label || source.title}</a>
											<!-- A note is the reader's own words. Labelled, because an
											     answer that quotes one must not look like the book
											     saying it. -->
											{#if source.kind === 'note'}<span class="found yours">your note</span>{/if}
											{#if source.foundBy}<span class="found">{source.foundBy}</span>{/if}
											{#if !source.cited}<span class="found">not used</span>{/if}
										</div>
										<p class="excerpt">{source.excerpt}</p>
									</li>
								{/each}
							</ol>
						{/if}
					{/if}

					{#if timing(message)}
						<p class="timing">{timing(message)}</p>
					{/if}
				</div>
			{/if}
		{/each}

		{#if asking && streamingIndex !== null && !messages[streamingIndex]?.content}
			<p class="searching">Reading your books…</p>
		{/if}
	</div>

	{#if error}
		<p class="error" class:busy={error.busy} role="alert">{error.message}</p>
	{/if}

	{#if pinnedBook}
		<!-- Above the box rather than inside the header, because it changes what
		     the next question means and that belongs next to where it is typed. -->
		<p class="pinned">
			<span>Asking about <strong>{pinnedBook.title || 'this book'}</strong></span>
			<button
				class="unpin"
				onclick={askWholeLibrary}
				title="Ask the whole library instead"
				aria-label="Ask the whole library instead">×</button
			>
		</p>
	{/if}

	<form
		class="composer"
		onsubmit={(e) => {
			e.preventDefault();
			void send();
		}}
	>
		<textarea
			bind:value={question}
			onkeydown={onKey}
			placeholder="What would you like to know?"
			rows="2"
			disabled={asking || canAsk === false}
		></textarea>
		<button class="button send" type="submit" disabled={asking || canAsk === false || !question.trim()}>
			{asking ? 'Asking…' : 'Ask'}
		</button>
	</form>
</div>

<style>
	.shell {
		display: flex;
		flex-direction: column;
		height: 100dvh;
		max-width: 48rem;
		margin: 0 auto;
		padding: var(--space-4);
	}

	header {
		flex-shrink: 0;
		margin-bottom: var(--space-4);
	}

	.back {
		display: inline-block;
		margin-bottom: var(--space-2);
		color: var(--fg-muted);
		font-size: var(--text-sm);
		text-decoration: none;
	}

	.back:hover {
		color: var(--fg);
	}

	h1 {
		margin: 0 0 var(--space-3);
		font-family: var(--font-serif);
		font-size: var(--text-2xl);
	}

	.row {
		display: flex;
		flex-wrap: wrap;
		gap: var(--space-2);
	}

	.threads {
		flex-shrink: 0;
		margin-bottom: var(--space-4);
		max-height: 12rem;
		overflow-y: auto;
		border: 1px solid var(--border);
		border-radius: var(--radius-md);
		background: var(--bg-raised);
	}

	.threads ul {
		list-style: none;
		margin: 0;
		padding: 0;
	}

	.threads li + li {
		border-top: 1px solid var(--border);
	}

	.threads button {
		width: 100%;
		min-height: 44px;
		padding: var(--space-2) var(--space-3);
		border: 0;
		background: transparent;
		color: var(--fg);
		font: inherit;
		text-align: left;
		cursor: pointer;
	}

	.threads button:hover {
		background: var(--bg-sunken);
	}

	.threads button.current {
		font-weight: 600;
	}

	.transcript {
		flex: 1;
		overflow-y: auto;
		padding-right: var(--space-2);
	}

	.intro {
		padding: var(--space-8) 0;
		max-width: var(--measure);
	}

	.intro p {
		margin: 0 0 var(--space-3);
	}

	.turn {
		margin-bottom: var(--space-4);
	}

	/* A question is styled as something said, an answer as something read: the
	   answer is the part with the typography of a book. */
	.question p {
		margin: 0 0 0 auto;
		max-width: 80%;
		width: fit-content;
		padding: var(--space-2) var(--space-3);
		border-radius: var(--radius-md);
		background: var(--bg-sunken);
	}

	.answer .body {
		font-family: var(--font-serif);
		font-size: var(--text-lg);
		line-height: var(--leading-reading);
	}

	/* Paragraphs and bullets are separated by the space between them rather
	   than by the literal newlines the model wrote, which is why there is no
	   pre-wrap here any more. */
	.answer .body :global(p) {
		margin: 0 0 var(--space-3);
	}

	.answer .body :global(p:last-child),
	.answer .body :global(ul:last-child) {
		margin-bottom: 0;
	}

	.answer .body :global(ul) {
		margin: 0 0 var(--space-3);
		padding-left: var(--space-6);
	}

	.answer .body :global(li) {
		margin-bottom: var(--space-1);
	}

	.answer .body :global(code) {
		padding: 0 0.2em;
		border-radius: var(--radius-sm);
		background: var(--bg-sunken);
		font-family: var(--font-mono);
		font-size: 0.9em;
	}

	.searching {
		margin: 0;
		padding: 0 0.25rem;
		color: var(--fg-muted);
		font-style: italic;
	}

	/* The cursor marks text that is still arriving. Without it a model that
	   pauses mid-sentence is indistinguishable from one that has finished and
	   stopped early. */
	.cursor {
		display: inline-block;
		width: 0.5rem;
		height: 1em;
		margin-left: 0.1rem;
		vertical-align: text-bottom;
		background: currentColor;
		opacity: 0.7;
		animation: blink 1.1s step-end infinite;
	}

	@keyframes blink {
		0%,
		100% {
			opacity: 0.7;
		}
		50% {
			opacity: 0;
		}
	}

	/* Someone who finds a blinking cursor distracting has usually said so at
	   the system level. */
	@media (prefers-reduced-motion: reduce) {
		.cursor {
			animation: none;
		}
	}

	/* A citation is a link into the book, and looks like one: small, raised,
	   and unmistakably clickable. Following it is the point of the whole
	   feature. */
	.cite {
		display: inline-block;
		padding: 0 var(--space-1);
		border-radius: var(--radius-sm);
		background: var(--accent);
		color: var(--accent-fg);
		font-family: var(--font-sans);
		font-size: var(--text-xs);
		vertical-align: super;
		text-decoration: none;
		line-height: 1.4;
	}

	.cite:hover {
		filter: brightness(1.15);
	}

	/* A note stands apart from the passages: it is this reader's own view,
	   sitting in a list of what the books said. */
	.sources li.mine {
		border-left: 3px solid var(--accent);
		padding-left: var(--space-2);
	}

	.found.yours {
		background: color-mix(in srgb, var(--accent) 22%, transparent);
	}

	.sources-toggle {
		margin-top: var(--space-3);
		padding: 0;
		border: 0;
		background: none;
		color: var(--fg-muted);
		font-size: var(--text-sm);
		text-decoration: underline;
		cursor: pointer;
	}

	.sources {
		margin: var(--space-3) 0 0;
		padding-left: var(--space-6);
	}

	.sources li {
		margin-bottom: var(--space-3);
	}

	.sources li.uncited {
		opacity: 0.65;
	}

	.source-head {
		display: flex;
		flex-wrap: wrap;
		align-items: baseline;
		gap: var(--space-2);
		font-size: var(--text-sm);
	}

	.source-head a {
		color: var(--fg);
	}

	.found {
		padding: 0 var(--space-2);
		border: 1px solid var(--border);
		border-radius: var(--radius-sm);
		color: var(--fg-subtle);
		font-size: var(--text-xs);
	}

	.excerpt {
		margin: var(--space-1) 0 0;
		color: var(--fg-muted);
		font-size: var(--text-sm);
	}

	.timing {
		margin: var(--space-2) 0 0;
		color: var(--fg-subtle);
		font-size: var(--text-xs);
	}

	.pinned {
		flex-shrink: 0;
		display: flex;
		align-items: center;
		justify-content: space-between;
		gap: var(--space-2);
		margin: 0;
		padding: var(--space-2) var(--space-3);
		border: 1px solid var(--border);
		border-radius: var(--radius-md);
		background: var(--bg-sunken);
		color: var(--fg-muted);
		font-size: var(--text-sm);
	}

	.pinned strong {
		color: var(--fg);
		font-weight: 600;
	}

	.unpin {
		flex-shrink: 0;
		/* 44px, because this is the one control on the screen that undoes a
		   scope somebody may not have chosen deliberately. */
		min-width: 44px;
		min-height: 44px;
		border: none;
		background: none;
		color: var(--fg-muted);
		font-size: var(--text-lg);
		line-height: 1;
		cursor: pointer;
	}

	.unpin:hover {
		color: var(--fg);
	}

	.composer {
		flex-shrink: 0;
		display: flex;
		gap: var(--space-2);
		padding-top: var(--space-3);
		border-top: 1px solid var(--border);
	}

	textarea {
		flex: 1;
		padding: var(--space-2) var(--space-3);
		border: 1px solid var(--border);
		border-radius: var(--radius-md);
		background: var(--bg-raised);
		color: var(--fg);
		font: inherit;
		resize: vertical;
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
		cursor: pointer;
	}

	.button:hover:not(:disabled) {
		background: var(--bg-sunken);
	}

	.button:disabled {
		opacity: 0.6;
		cursor: default;
	}

	.send {
		align-self: flex-end;
	}

	.error {
		flex-shrink: 0;
		margin: var(--space-2) 0 0;
		color: var(--danger);
		font-size: var(--text-sm);
	}

	/* A busy model is a wait, not a fault: said plainly, not in red. */
	.error.busy {
		color: var(--fg-muted);
	}

	.empty,
	.hint {
		color: var(--fg-muted);
		font-size: var(--text-sm);
	}
</style>

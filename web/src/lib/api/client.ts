// SPDX-License-Identifier: AGPL-3.0-or-later

/**
 * Typed client for the Lexarii API.
 *
 * The wire types are NOT written here. They are aliases into `types.gen.ts`,
 * which openapi-typescript generates from `api/openapi.yaml`, whose schemas are
 * in turn read out of the Go structs in internal/http/dto. So a field renamed
 * in Go becomes a TypeScript compile error rather than an undefined at runtime,
 * which is the entire reason the chain exists.
 *
 * Only the fetch helpers and the few client-side shapes that have no server
 * counterpart - query builders, streaming callbacks - are written by hand.
 *
 * After changing anything in internal/http/dto: `make api-spec && make api-types`.
 */

import type { components } from './types.gen';

export type UploadStatus = components['schemas']['UploadStatus'];

export type LibraryItem = components['schemas']['LibraryItem'];

export type LibraryPage = components['schemas']['LibraryPage'];

export type Author = components['schemas']['Author'];

export type BookFile = components['schemas']['BookFile'];

export type Tag = components['schemas']['Tag'];

export type WorkDetail = components['schemas']['WorkDetail'];

export type UpdateWork = components['schemas']['UpdateWork'];

export type Stats = components['schemas']['Stats'];

export type DuplicateGroup = components['schemas']['DuplicateGroup'];

export type DuplicateWork = components['schemas']['DuplicateWork'];

export type Mention = components['schemas']['Mention'];

export type MentionList = components['schemas']['MentionList'];

export type Job = components['schemas']['Job'];

export type JobAccepted = components['schemas']['JobAccepted'];

export type UploadedFile = components['schemas']['UploadedFile'];

export type UploadResult = components['schemas']['UploadResult'];

export interface LibraryQuery {
	search?: string;
	tag?: string;
	sort?: 'title' | 'added' | 'author' | 'series';
	status?: string;
	includeMissing?: boolean;
	/** Whose books: omitted for everything readable, or one side of that. */
	owned?: 'mine' | 'shared';
	limit?: number;
	cursor?: string;
}

/** ApiError carries the server's machine-readable code alongside its message. */
export class ApiError extends Error {
	constructor(
		readonly code: string,
		message: string,
		readonly status: number
	) {
		super(message);
		this.name = 'ApiError';
	}
}

async function request<T>(path: string, init?: RequestInit): Promise<T> {
	const response = await fetch(path, init);

	if (!response.ok) {
		// Every endpoint uses one error shape. Falling back to the status text
		// covers a proxy error page, which is not the server's JSON.
		let code = 'http_' + response.status;
		let message = response.statusText;
		try {
			const body = await response.json();
			if (body?.error) {
				code = body.error.code ?? code;
				message = body.error.message ?? message;
			}
		} catch {
			/* not JSON; keep the status text */
		}
		throw new ApiError(code, message, response.status);
	}

	if (response.status === 204) {
		return undefined as T;
	}
	return response.json() as Promise<T>;
}

export function library(query: LibraryQuery = {}, signal?: AbortSignal): Promise<LibraryPage> {
	const params = new URLSearchParams();
	if (query.search) params.set('search', query.search);
	if (query.tag) params.set('tag', query.tag);
	if (query.sort) params.set('sort', query.sort);
	if (query.status) params.set('status', query.status);
	if (query.includeMissing) params.set('includeMissing', 'true');
	if (query.owned) params.set('owned', query.owned);
	if (query.limit) params.set('limit', String(query.limit));
	if (query.cursor) params.set('cursor', query.cursor);

	const qs = params.toString();
	return request<LibraryPage>('/api/v1/library' + (qs ? '?' + qs : ''), {
		signal
	});
}

export function work(id: string, signal?: AbortSignal): Promise<WorkDetail> {
	return request<WorkDetail>('/api/v1/works/' + encodeURIComponent(id), {
		signal
	});
}

/**
 * The books in this library that name this one.
 *
 * Its own request, not part of the work detail: most books have none, the
 * search costs a full-text query, and this sits below the fold.
 */
export function mentions(id: string, signal?: AbortSignal): Promise<MentionList> {
	return request<MentionList>('/api/v1/works/' + encodeURIComponent(id) + '/mentions', {
		signal
	});
}

/**
 * Corrects a book's details by hand.
 *
 * A field LEFT OUT is left alone; a field sent empty is cleared. So the caller
 * must send exactly what it means - which is why the form below builds its
 * patch from what changed rather than submitting every box on the page.
 */
export function editWork(id: string, edit: UpdateWork): Promise<WorkDetail> {
	return request<WorkDetail>('/api/v1/works/' + encodeURIComponent(id), {
		method: 'PATCH',
		headers: { 'Content-Type': 'application/json' },
		body: JSON.stringify(edit)
	});
}

/** Books that look like one book under two filenames. */
export function duplicates(signal?: AbortSignal): Promise<{ items: DuplicateGroup[] }> {
	return request<{ items: DuplicateGroup[] }>('/api/v1/duplicates', { signal });
}

/**
 * Folds books into one. The id is the copy being KEPT.
 *
 * Nothing merges on its own: two printings and a translation look identical to
 * any rule, and welding them together loses a library in a way no undo helps.
 */
export function mergeWorks(keep: string, merge: string[]): Promise<{ filesMoved: number }> {
	return request<{ filesMoved: number }>('/api/v1/works/' + encodeURIComponent(keep) + '/merge', {
		method: 'POST',
		headers: { 'Content-Type': 'application/json' },
		body: JSON.stringify({ merge })
	});
}

export function stats(signal?: AbortSignal): Promise<Stats> {
	return request<Stats>('/api/v1/stats', { signal });
}

export function startScan(): Promise<JobAccepted> {
	return request<JobAccepted>('/api/v1/scan', { method: 'POST' });
}

export function job(id: string, signal?: AbortSignal): Promise<Job> {
	return request<Job>('/api/v1/jobs/' + encodeURIComponent(id), { signal });
}

export function upload(files: File[], signal?: AbortSignal): Promise<UploadResult> {
	const body = new FormData();
	for (const file of files) {
		body.append('files', file, file.name);
	}
	return request<UploadResult>('/api/v1/uploads', {
		method: 'POST',
		body,
		signal
	});
}

/**
 * Follows a job to completion over SSE.
 *
 * The server guarantees a terminal `done` event, so this always settles. The
 * `error` event and a transport failure both reject; everything else calls
 * `onProgress`.
 */
export function followJob(
	id: string,
	onProgress: (job: Job) => void,
	signal?: AbortSignal
): Promise<Job> {
	return new Promise((resolve, reject) => {
		const source = new EventSource('/api/v1/jobs/' + encodeURIComponent(id) + '/events');

		const close = () => source.close();
		signal?.addEventListener('abort', () => {
			close();
			reject(new DOMException('aborted', 'AbortError'));
		});

		source.addEventListener('progress', (event) => {
			onProgress(JSON.parse((event as MessageEvent).data) as Job);
		});

		source.addEventListener('done', (event) => {
			const final = JSON.parse((event as MessageEvent).data) as Job;
			onProgress(final);
			close();
			resolve(final);
		});

		source.addEventListener('error', (event) => {
			// EventSource fires a bare `error` on a dropped connection as well as
			// on the server's own error event, which carries data.
			const data = (event as MessageEvent).data;
			close();
			if (data) {
				reject(new Error(JSON.parse(data).message ?? 'the job reported an error'));
			} else {
				reject(new Error('lost the connection while following this job'));
			}
		});
	});
}

export type SpineItem = components['schemas']['SpineItem'];

export type TOCEntry = components['schemas']['TOCEntry'];

export type ReaderManifest = components['schemas']['ReaderManifest'];

export type SpineContent = components['schemas']['SpineContent'];

export function readerManifest(bookFileId: string, signal?: AbortSignal): Promise<ReaderManifest> {
	return request<ReaderManifest>('/api/v1/books/' + encodeURIComponent(bookFileId) + '/manifest', {
		signal
	});
}

export function spineContent(
	bookFileId: string,
	index: number,
	signal?: AbortSignal
): Promise<SpineContent> {
	return request<SpineContent>(
		'/api/v1/books/' + encodeURIComponent(bookFileId) + '/spine/' + index,
		{ signal }
	);
}

export type ReadingProgress = components['schemas']['ReadingProgress'];

export function readingProgress(
	bookFileId: string,
	signal?: AbortSignal
): Promise<ReadingProgress> {
	return request<ReadingProgress>('/api/v1/books/' + encodeURIComponent(bookFileId) + '/progress', {
		signal
	});
}

export function saveProgress(
	bookFileId: string,
	locator: string,
	percent?: number
): Promise<ReadingProgress> {
	return request<ReadingProgress>('/api/v1/books/' + encodeURIComponent(bookFileId) + '/progress', {
		method: 'PUT',
		headers: { 'Content-Type': 'application/json' },
		body: JSON.stringify({ locator, percent })
	});
}

export type BookInfo = components['schemas']['BookInfo'];

export function bookInfo(bookFileId: string, signal?: AbortSignal): Promise<BookInfo> {
	return request<BookInfo>('/api/v1/books/' + encodeURIComponent(bookFileId) + '/info', { signal });
}

export type NoteShareList = components['schemas']['NoteShareList'];

/**
 * Who you share your own notes on this book with.
 *
 * Yours only. Who else has shared notes on the same book is not yours to see:
 * it would answer a question about the owner's other readers.
 */
export function noteShares(bookFileId: string, signal?: AbortSignal): Promise<NoteShareList> {
	return request<NoteShareList>(
		'/api/v1/books/' + encodeURIComponent(bookFileId) + '/annotations/shares',
		{ signal }
	);
}

/** Shares your notes on this book, or stops. Sharing yours needs nobody's permission. */
export function setNoteShare(bookFileId: string, userId: string, on: boolean): Promise<void> {
	return request<void>(
		'/api/v1/books/' +
			encodeURIComponent(bookFileId) +
			'/annotations/shares/' +
			encodeURIComponent(userId),
		{ method: on ? 'PUT' : 'DELETE' }
	);
}

export type Subject = components['schemas']['Subject'];

export type SubjectList = components['schemas']['SubjectList'];

/** What this library is about, commonest first, over the books you can read. */
export function subjects(signal?: AbortSignal): Promise<SubjectList> {
	return request<SubjectList>('/api/v1/subjects', { signal });
}

export type AccountDeleted = components['schemas']['AccountDeleted'];

export type ArchivedBook = components['schemas']['ArchivedBook'];

export type ArchivedBooks = components['schemas']['ArchivedBooks'];

/**
 * Deletes an account.
 *
 * An administrator may delete anybody; anybody may delete themselves. The
 * person goes immediately - notes, positions, conversations - and the books
 * they owned are archived, which is what the reply counts.
 */
export function deleteAccount(userId: string): Promise<AccountDeleted> {
	return request<AccountDeleted>('/api/v1/users/' + encodeURIComponent(userId), {
		method: 'DELETE'
	});
}

/** Books left behind by deleted accounts. Administrators only. */
export function archivedBooks(signal?: AbortSignal): Promise<ArchivedBooks> {
	return request<ArchivedBooks>('/api/v1/archive', { signal });
}

/** Gives an archived book a new owner. Omit the owner to claim it yourself. */
export function claimArchived(bookFileId: string, ownerId?: string): Promise<void> {
	return request<void>('/api/v1/archive/' + encodeURIComponent(bookFileId) + '/claim', {
		method: 'POST',
		headers: { 'Content-Type': 'application/json' },
		body: JSON.stringify(ownerId ? { ownerId } : {})
	});
}

export type RemoveForEveryoneRequest = components['schemas']['RemoveForEveryoneRequest'];

export type RemovedForEveryone = components['schemas']['RemovedForEveryone'];

export type BlockedFile = components['schemas']['BlockedFile'];

export type BlockedFiles = components['schemas']['BlockedFiles'];

export type DiscardedCopies = components['schemas']['DiscardedCopies'];

/**
 * Takes a file off the installation for everybody and blocks its bytes.
 * Administrators only. The reply counts the people it was taken from.
 * `purgeQuotes` also erases the file's words quoted in highlights and chat
 * citations, keeping the marks and citations themselves.
 */
export function removeForEveryone(
	bookFileId: string,
	reason: string,
	purgeQuotes = false
): Promise<RemovedForEveryone> {
	const body: RemoveForEveryoneRequest = { reason, purgeQuotes };
	return request<RemovedForEveryone>(
		'/api/v1/admin/books/' + encodeURIComponent(bookFileId) + '/remove',
		{
			method: 'POST',
			headers: { 'Content-Type': 'application/json' },
			body: JSON.stringify(body)
		}
	);
}

/** Files removed for everybody and kept off. Administrators only. */
export function blockedFiles(signal?: AbortSignal): Promise<BlockedFiles> {
	return request<BlockedFiles>('/api/v1/admin/blocked', { signal });
}

/** Finishes deleting a removed file's stored copies. Administrators only. */
export function discardCopies(contentHash: string): Promise<DiscardedCopies> {
	return request<DiscardedCopies>(
		'/api/v1/admin/blocked/' + encodeURIComponent(contentHash) + '/discard',
		{ method: 'POST' }
	);
}

/** Lifts a block. Nothing comes back by itself. Administrators only. */
export function unblock(contentHash: string): Promise<void> {
	return request<void>('/api/v1/admin/blocked/' + encodeURIComponent(contentHash), {
		method: 'DELETE'
	});
}

export type ShareActivity = components['schemas']['ShareActivity'];

export type SharingActivity = components['schemas']['SharingActivity'];

/**
 * What has happened lately to the books this person lent.
 *
 * Owner-scoped by the server: it reports on YOUR books, so there is nothing to
 * pass. Reading your own library is never logged at all, so an empty answer is
 * the normal one on an installation where nothing is shared.
 */
export function sharingActivity(signal?: AbortSignal): Promise<SharingActivity> {
	return request<SharingActivity>('/api/v1/shares/activity', { signal });
}

export type AuthProvider = components['schemas']['AuthProvider'];

export type AuthOptions = components['schemas']['AuthOptions'];

export type SignInLink = components['schemas']['SignInLink'];

export type User = components['schemas']['User'];

export function authOptions(signal?: AbortSignal): Promise<AuthOptions> {
	return request<AuthOptions>('/api/v1/auth/providers', { signal });
}

export function me(signal?: AbortSignal): Promise<User> {
	return request<User>('/api/v1/auth/me', { signal });
}

export function login(username: string, password: string, provider?: string): Promise<User> {
	return request<User>('/api/v1/auth/login', {
		method: 'POST',
		headers: { 'Content-Type': 'application/json' },
		body: JSON.stringify({ username, password, provider })
	});
}

export function setup(username: string, password: string, email?: string): Promise<User> {
	return request<User>('/api/v1/auth/setup', {
		method: 'POST',
		headers: { 'Content-Type': 'application/json' },
		body: JSON.stringify({ username, password, email })
	});
}

export function logout(): Promise<void> {
	return request<void>('/api/v1/auth/logout', { method: 'POST' });
}

/** One passage an answer drew on. */
export type Source = components['schemas']['Source'];

export type AnswerBook = components['schemas']['AnswerBook'];

export type Answer = components['schemas']['Answer'];

export type ChatMessage = components['schemas']['Message'];

export type Conversation = components['schemas']['Conversation'];

export type ConversationSummary = components['schemas']['ConversationSummary'];

export function conversations(signal?: AbortSignal): Promise<{ items: ConversationSummary[] }> {
	return request<{ items: ConversationSummary[] }>('/api/v1/conversations', {
		signal
	});
}

export function conversation(id: string, signal?: AbortSignal): Promise<Conversation> {
	return request<Conversation>('/api/v1/conversations/' + encodeURIComponent(id), { signal });
}

export function deleteConversation(id: string): Promise<void> {
	return request<void>('/api/v1/conversations/' + encodeURIComponent(id), {
		method: 'DELETE'
	});
}

/** Asks a question. Omit the conversation id to start a new thread. */
export function ask(
	question: string,
	conversationId?: string,
	bookFileId?: string
): Promise<Answer> {
	const path = conversationId
		? '/api/v1/conversations/' + encodeURIComponent(conversationId) + '/messages'
		: '/api/v1/conversations';
	return request<Answer>(path, {
		method: 'POST',
		headers: { 'Content-Type': 'application/json' },
		body: JSON.stringify({ question, bookFileId })
	});
}

/** What a streaming answer reports as it arrives. */
export interface AskHandlers {
	/** The thread this question landed in, before the answer starts. */
	onConversation?: (summary: ConversationSummary) => void;
	/** The passages the answer will be built from, seconds before the text. */
	onPassages?: (sources: Source[]) => void;
	/** One piece of the answer. */
	onDelta?: (text: string) => void;
}

/** The status a request would have had, for the codes a stream can end with. */
const streamFailureStatus: Record<string, number> = {
	llm_unavailable: 503,
	model_error: 502
};

/**
 * Asks a question and reports the answer as it is written.
 *
 * EventSource cannot POST and cannot send a body, so the stream is read from
 * the response directly and the SSE framing is parsed here. That is a dozen
 * lines and avoids inventing a GET endpoint that takes a question in the query
 * string, where it would end up in every access log.
 *
 * Resolves with the complete Answer, which is also what gets stored, so the
 * caller never has to reassemble the text from the pieces it was shown.
 */
export async function askStreaming(
	question: string,
	handlers: AskHandlers,
	conversationId?: string,
	bookFileId?: string,
	signal?: AbortSignal
): Promise<Answer> {
	const path = conversationId
		? '/api/v1/conversations/' + encodeURIComponent(conversationId) + '/messages/stream'
		: '/api/v1/conversations/stream';

	const response = await fetch(path, {
		method: 'POST',
		headers: {
			'Content-Type': 'application/json',
			Accept: 'text/event-stream'
		},
		body: JSON.stringify({ question, bookFileId }),
		signal
	});

	if (!response.ok || !response.body) {
		// Errors arrive before the stream opens, in the usual shape.
		let code = 'http_' + response.status;
		let message = response.statusText;
		try {
			const body = await response.json();
			if (body?.error) {
				code = body.error.code ?? code;
				message = body.error.message ?? message;
			}
		} catch {
			/* not JSON; keep the status text */
		}
		throw new ApiError(code, message, response.status);
	}

	const reader = response.body.pipeThrough(new TextDecoderStream()).getReader();
	let buffer = '';
	let answer: Answer | null = null;
	let failure: { code?: string; message?: string } | null = null;

	try {
		for (;;) {
			const { done, value } = await reader.read();
			if (done) break;
			buffer += value;

			// Events are separated by a blank line. A chunk can end mid-event,
			// so whatever follows the last separator stays in the buffer.
			const parts = buffer.split('\n\n');
			buffer = parts.pop() ?? '';

			for (const part of parts) {
				let name = '';
				let data = '';
				for (const line of part.split('\n')) {
					if (line.startsWith('event: ')) name = line.slice(7);
					else if (line.startsWith('data: ')) data += line.slice(6);
				}
				if (!name || !data) continue;

				let payload: unknown;
				try {
					payload = JSON.parse(data);
				} catch {
					continue;
				}

				switch (name) {
					case 'conversation':
						handlers.onConversation?.(payload as ConversationSummary);
						break;
					case 'passages':
						handlers.onPassages?.(payload as Source[]);
						break;
					case 'delta':
						handlers.onDelta?.((payload as { text: string }).text);
						break;
					case 'done':
						answer = payload as Answer;
						break;
					case 'error':
						// The code and message the request would have failed
						// with, had the stream not already opened.
						failure = payload as { code?: string; message?: string };
						break;
				}
			}
		}
	} finally {
		// Releasing the lock lets an aborted fetch tear the connection down
		// rather than leaving it open until the model finishes.
		reader.releaseLock();
	}

	if (failure) {
		const code = failure.code || 'answer_failed';
		throw new ApiError(
			code,
			failure.message || 'the answer failed',
			streamFailureStatus[code] ?? 500
		);
	}
	if (!answer) throw new ApiError('answer_incomplete', 'the answer ended before it finished', 500);
	return answer;
}

export type Person = components['schemas']['Person'];
export type Share = components['schemas']['Share'];

/** The accounts a book can be shared with. */
export function people(signal?: AbortSignal): Promise<{ items: Person[] }> {
	return request<{ items: Person[] }>('/api/v1/people', { signal });
}

/** Who a book is shared with. Only its owner may ask. */
export function shares(bookFileId: string, signal?: AbortSignal): Promise<{ items: Share[] }> {
	return request<{ items: Share[] }>(
		'/api/v1/books/' + encodeURIComponent(bookFileId) + '/shares',
		{ signal }
	);
}

/**
 * Sets one person's access to one book.
 *
 * Idempotent, so the caller says what the access should BE rather than having
 * to know whether a share already exists.
 */
export function share(
	bookFileId: string,
	userId: string,
	access: { canDownload: boolean; annotations: boolean }
): Promise<void> {
	return request<void>(
		'/api/v1/books/' + encodeURIComponent(bookFileId) + '/shares/' + encodeURIComponent(userId),
		{
			method: 'PUT',
			headers: { 'Content-Type': 'application/json' },
			body: JSON.stringify(access)
		}
	);
}

export type Recipient = components['schemas']['Recipient'];

/**
 * Lends a book to the account with this username, for somebody not shown a
 * list of people. Answers the share made, with the account's id; an unknown
 * name is an ApiError with code `user_not_found`.
 */
export function shareByUsername(
	bookFileId: string,
	username: string,
	access: { canDownload: boolean; annotations: boolean }
): Promise<Share> {
	return request<Share>('/api/v1/books/' + encodeURIComponent(bookFileId) + '/shares', {
		method: 'POST',
		headers: { 'Content-Type': 'application/json' },
		body: JSON.stringify({ username, ...access })
	});
}

/** Shares your notes on this book with the account with this username. */
export function shareNotesByUsername(bookFileId: string, username: string): Promise<Recipient> {
	return request<Recipient>(
		'/api/v1/books/' + encodeURIComponent(bookFileId) + '/annotations/shares',
		{
			method: 'POST',
			headers: { 'Content-Type': 'application/json' },
			body: JSON.stringify({ username })
		}
	);
}

/**
 * Takes a book off the caller's own shelf: they stop holding this file.
 *
 * Their notes and reading position on it are kept. Whether anybody else still
 * holds it is deliberately not reported.
 */
export function releaseHolding(bookFileId: string): Promise<void> {
	return request<void>('/api/v1/books/' + encodeURIComponent(bookFileId) + '/holding', {
		method: 'DELETE'
	});
}

/** Withdraws someone's access. Their view of the owner's notes goes with it. */
export function unshare(bookFileId: string, userId: string): Promise<void> {
	return request<void>(
		'/api/v1/books/' + encodeURIComponent(bookFileId) + '/shares/' + encodeURIComponent(userId),
		{ method: 'DELETE' }
	);
}

export type AccessRecord = components['schemas']['AccessRecord'];
export type CoverTarget = components['schemas']['CoverTarget'];
export type ContinueItem = components['schemas']['ContinueItem'];

/**
 * Books this reader has started and not finished, most recent first.
 *
 * The endpoint has existed since reading position was added and nothing called
 * it, so the shelf never showed what you were in the middle of - the one thing
 * a reader opens the app to find.
 */
export function continueReading(signal?: AbortSignal): Promise<{ items: ContinueItem[] }> {
	return request<{ items: ContinueItem[] }>('/api/v1/continue', { signal });
}

/**
 * Books that still need a cover rendered.
 *
 * `remaining` counts the whole library, not the batch: the server hands out 24
 * at a time, so a client that counted what it was given would report the same
 * number after every round.
 */
export function pendingCovers(
	signal?: AbortSignal
): Promise<{ items: CoverTarget[]; remaining: number }> {
	return request<{ items: CoverTarget[]; remaining: number }>('/api/v1/covers/pending', { signal });
}

/**
 * Stores a cover rendered by the client.
 *
 * The server sniffs the bytes rather than trusting the content type, so sending
 * something that is not an image is refused rather than stored.
 */
export function putCover(bookFileId: string, image: Blob): Promise<void> {
	return request<void>('/api/v1/books/' + encodeURIComponent(bookFileId) + '/cover', {
		method: 'PUT',
		headers: { 'Content-Type': image.type || 'image/webp' },
		body: image
	});
}

/**
 * Who has read a book, and how much of it. Only its owner may ask.
 *
 * Reading your own library is never recorded, so this is always about other
 * people - which is what makes it an audit trail rather than surveillance.
 */
export function accessLog(
	bookFileId: string,
	signal?: AbortSignal
): Promise<{ items: AccessRecord[] }> {
	return request<{ items: AccessRecord[] }>(
		'/api/v1/books/' + encodeURIComponent(bookFileId) + '/access',
		{ signal }
	);
}

/** Something a model proposed, awaiting a decision. */
export type Suggestion = components['schemas']['Suggestion'];

export type EnrichmentStarted = components['schemas']['EnrichmentStarted'];

export function startEnrichment(limit?: number): Promise<EnrichmentStarted> {
	return request<EnrichmentStarted>('/api/v1/enrich', {
		method: 'POST',
		headers: { 'Content-Type': 'application/json' },
		body: JSON.stringify({ limit })
	});
}

export function suggestions(signal?: AbortSignal): Promise<{ items: Suggestion[] }> {
	return request<{ items: Suggestion[] }>('/api/v1/suggestions', { signal });
}

export function resolveSuggestion(id: string, accept: boolean): Promise<void> {
	return request<void>(
		'/api/v1/suggestions/' + encodeURIComponent(id) + (accept ? '/accept' : '/reject'),
		{ method: 'POST' }
	);
}

/** One live sign-in. */
export type Session = components['schemas']['Session'];

export function sessions(signal?: AbortSignal): Promise<{ items: Session[] }> {
	return request<{ items: Session[] }>('/api/v1/auth/sessions', { signal });
}

export function revokeSession(id: string): Promise<void> {
	return request<void>('/api/v1/auth/sessions/' + encodeURIComponent(id), {
		method: 'DELETE'
	});
}

/** Ends every session, this one included. */
export function revokeAllSessions(): Promise<void> {
	return request<void>('/api/v1/auth/sessions/revoke-all', { method: 'POST' });
}

export type Annotation = components['schemas']['Annotation'];

export function annotations(
	bookFileId: string,
	signal?: AbortSignal
): Promise<{ items: Annotation[] }> {
	return request<{ items: Annotation[] }>(
		'/api/v1/books/' + encodeURIComponent(bookFileId) + '/annotations',
		{ signal }
	);
}

export type ImportedMark = components['schemas']['ImportedMark'];

/**
 * Stores the highlights read out of a PDF's own file.
 *
 * Replaces whatever the last import produced for this book; marks made in the
 * reader are untouched.
 */
export function importMarks(
	bookFileId: string,
	marks: ImportedMark[]
): Promise<{ imported: number; offered: number }> {
	return request<{ imported: number; offered: number }>(
		'/api/v1/books/' + encodeURIComponent(bookFileId) + '/annotations/import',
		{
			method: 'POST',
			headers: { 'Content-Type': 'application/json' },
			body: JSON.stringify({ marks })
		}
	);
}

export type Collection = components['schemas']['Collection'];
export type CollectionDetail = components['schemas']['CollectionDetail'];

/** The reader's own groupings of their books. */
export function collections(signal?: AbortSignal): Promise<{ items: Collection[] }> {
	return request<{ items: Collection[] }>('/api/v1/collections', { signal });
}

export function collection(id: string, signal?: AbortSignal): Promise<CollectionDetail> {
	return request<CollectionDetail>('/api/v1/collections/' + encodeURIComponent(id), { signal });
}

export function createCollection(name: string, description?: string): Promise<Collection> {
	return request<Collection>('/api/v1/collections', {
		method: 'POST',
		headers: { 'Content-Type': 'application/json' },
		body: JSON.stringify({ name, description })
	});
}

export function renameCollection(
	id: string,
	name: string,
	description?: string
): Promise<Collection> {
	return request<Collection>('/api/v1/collections/' + encodeURIComponent(id), {
		method: 'PATCH',
		headers: { 'Content-Type': 'application/json' },
		body: JSON.stringify({ name, description })
	});
}

export function deleteCollection(id: string): Promise<void> {
	return request<void>('/api/v1/collections/' + encodeURIComponent(id), { method: 'DELETE' });
}

/** Idempotent: a book already in the collection is success. */
export function addToCollection(id: string, workId: string): Promise<void> {
	return request<void>(
		'/api/v1/collections/' + encodeURIComponent(id) + '/works/' + encodeURIComponent(workId),
		{ method: 'PUT' }
	);
}

export function removeFromCollection(id: string, workId: string): Promise<void> {
	return request<void>(
		'/api/v1/collections/' + encodeURIComponent(id) + '/works/' + encodeURIComponent(workId),
		{ method: 'DELETE' }
	);
}

export type RecentAnnotation = components['schemas']['RecentAnnotation'];

/**
 * Every mark this reader has made, newest first, across the whole library, a
 * page at a time: pass the previous page's `nextCursor` for the next one.
 *
 * The endpoint and its DTO have existed since annotations did - the DTO's own
 * comment calls it "the notes view across the whole library" - and nothing
 * ever called it, so the only way to find a highlight was to remember which
 * book it was in and turn to the page.
 */
export type RecentAnnotationList = components['schemas']['RecentAnnotationList'];

export function recentAnnotations(
	page: { limit?: number; cursor?: string } = {},
	signal?: AbortSignal
): Promise<RecentAnnotationList> {
	const params = new URLSearchParams();
	if (page.limit) params.set('limit', String(page.limit));
	if (page.cursor) params.set('cursor', page.cursor);
	const qs = params.toString();
	return request<RecentAnnotationList>('/api/v1/annotations' + (qs ? '?' + qs : ''), { signal });
}

export function createAnnotation(
	bookFileId: string,
	body: {
		type?: string;
		start: string;
		end: string;
		selectedText?: string;
		note?: string;
		colour?: string;
	}
): Promise<Annotation> {
	return request<Annotation>('/api/v1/books/' + encodeURIComponent(bookFileId) + '/annotations', {
		method: 'POST',
		headers: { 'Content-Type': 'application/json' },
		body: JSON.stringify(body)
	});
}

export function updateAnnotation(id: string, note: string, colour?: string): Promise<Annotation> {
	return request<Annotation>('/api/v1/annotations/' + encodeURIComponent(id), {
		method: 'PATCH',
		headers: { 'Content-Type': 'application/json' },
		body: JSON.stringify({ note, colour })
	});
}

export function deleteAnnotation(id: string): Promise<void> {
	return request<void>('/api/v1/annotations/' + encodeURIComponent(id), {
		method: 'DELETE'
	});
}

/** Reports anchors the reader could not resolve, so they are flagged not lost. */
export function reportOrphans(
	bookFileId: string,
	ids: string[],
	resolved: string[] = []
): Promise<{ orphaned: number }> {
	return request<{ orphaned: number }>(
		'/api/v1/books/' + encodeURIComponent(bookFileId) + '/annotations/orphaned',
		{
			method: 'POST',
			headers: { 'Content-Type': 'application/json' },
			body: JSON.stringify({ ids, resolved })
		}
	);
}

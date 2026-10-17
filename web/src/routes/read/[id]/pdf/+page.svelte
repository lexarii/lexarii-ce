<!-- SPDX-License-Identifier: AGPL-3.0-or-later -->
<script lang="ts">
	import { page } from '$app/state';
	import * as api from '$api/client';
	import { isTyping } from '$lib/reader/keys';
	import { occurrenceIn, rangeOf } from '$lib/reader/pdftext';
	import type { Annotation, TOCEntry } from '$api/client';

	/**
	 * The fixed-page reader.
	 *
	 * A PDF is not reflowable: its pages are a fixed layout the publisher chose,
	 * so the typography controls that make the EPUB reader work do not apply
	 * here. What matters instead is legibility of a page designed for paper on a
	 * screen, which is zoom, fit, and contrast.
	 *
	 * pdf.js is imported dynamically. It is 400 KB, and loading it on the
	 * library grid - which most sessions never leave - would cost every user for
	 * a feature only some of them reach.
	 */

	let pageNumber = $state(1);
	let pageCount = $state(0);
	// True when this book is being read a page at a time because it was shared
	// to read rather than to keep.
	let perPage = $state(false);
	// eslint-disable-next-line @typescript-eslint/no-explicit-any
	let lib: any = null;
	let loading = $state(true);
	// Whether opening has taken long enough to be worth explaining.
	//
	// The first page of a large PDF is slow because the whole document is being
	// read to find it: measured on a real library, 2.8 seconds for a 124 MB book
	// and 5.9 for a 112 MB one, against 13ms for every page after. "Opening…"
	// for six seconds looks like something is broken rather than like work
	// happening, and the one thing that makes the wait bearable is knowing it
	// happens once. The delay before saying so keeps a fast open quiet: an
	// apology that appears on every book teaches people the app is slow.
	let slowToOpen = $state(false);
	// Whether the first page has been painted yet.
	//
	// `loading` ends when the document is open, which for a book shared to be
	// READ is almost immediately: there is no document to open here, only a page
	// count, and the parse happens server-side on the first page request inside
	// renderPage. So the seconds this is about fell in a gap where the reader
	// showed nothing at all - the message had gone and the canvas was still
	// blank.
	let awaitingFirstPage = $state(true);
	let error = $state<string | null>(null);

	// A failure that does not cost the reader their book. See the EPUB reader:
	// `error` replaces the whole reading surface, which is right for a book
	// that would not open and wrong for a highlight that would not save.
	let notice = $state<string | null>(null);
	let rendering = $state(false);

	let canvas = $state<HTMLCanvasElement | null>(null);
	let textLayer = $state<HTMLDivElement | null>(null);
	let highlightLayer = $state<HTMLDivElement | null>(null);
	let container = $state<HTMLElement | null>(null);

	let chromeVisible = $state(true);
	let showSettings = $state(false);
	let showToc = $state(false);

	/**
	 * The document outline, with each entry's page.
	 *
	 * Resolved server-side during ingest, because it means following destination
	 * references through the PDF object graph - work the reader should not repeat
	 * on every open, and which pdf.js would make us do again in the browser.
	 */
	let toc = $state<TOCEntry[]>([]);

	type Prefs = { fit: 'width' | 'page'; invert: boolean; zoom: number };
	// Whole-page fit by default, not fit-to-width.
	//
	// Fit-to-width makes a page taller than the screen, so reading it means
	// scrolling - and the edges of that same screen turn the page. The two
	// gestures fight: scroll down, tap to continue, and the next page arrives
	// having silently skipped the part that was still below the fold. Whole-page
	// fit keeps one tap meaning exactly one page. Fit-to-width is still there
	// for anyone who wants larger text and is willing to scroll.
	const defaults: Prefs = { fit: 'page', invert: false, zoom: 1 };
	let prefs = $state<Prefs>({ ...defaults });

	// eslint-disable-next-line @typescript-eslint/no-explicit-any -- pdf.js has no bundled types here
	// $state because the chrome asks whether the whole book is open: importing
	// a PDF's own annotations means reading the file, which a book served a
	// page at a time never hands over.
	// eslint-disable-next-line @typescript-eslint/no-explicit-any
	let pdf = $state<any>(null);
	let renderTask: { cancel: () => void } | null = null;

	function loadPrefs() {
		try {
			const raw = localStorage.getItem('lexarii.pdf');
			if (raw) prefs = { ...defaults, ...JSON.parse(raw) };
		} catch {
			/* defaults are fine */
		}
	}

	function savePrefs() {
		try {
			localStorage.setItem('lexarii.pdf', JSON.stringify(prefs));
		} catch {
			/* not worth interrupting reading over */
		}
	}

	/** The page named by a `pdf:<page>:<offset>` locator, or null. */
	function pageFromLocator(locator: string): number | null {
		const match = /^pdf:(\d+):(\d+)$/.exec(locator);
		if (!match) return null;
		const target = Number(match[1]);
		return target >= 1 ? target : null;
	}

	/**
	 * The page a citation asked for, if this was opened from one.
	 *
	 * Nothing clears it from the URL: leaving it there means the link can be
	 * shared or reloaded and still arrive in the right place, which is what a
	 * reference is for.
	 */
	function citedPage(): number | null {
		const at = page.url.searchParams.get('at');
		return at ? pageFromLocator(at) : null;
	}

	async function open(id: string) {
		const explain = setTimeout(() => {
			slowToOpen = awaitingFirstPage;
		}, 1200);
		try {
			const pdfjs = await import('pdfjs-dist');
			const worker = await import('pdfjs-dist/build/pdf.worker.min.mjs?url');
			pdfjs.GlobalWorkerOptions.workerSrc = worker.default;
			lib = pdfjs;

			// A book shared to be READ is never handed over as a file, so the
			// whole-document endpoint refuses it. Those are read a page at a
			// time instead: slower to turn, and the only way the book can be
			// shown at all without giving it away.
			const info = await api.bookInfo(id);
			perPage = !info.canDownload;

			if (perPage) {
				// The page count comes from the catalogue rather than the
				// document, because there is no document here to count.
				pageCount = info.unitCount ?? 0;
				if (pageCount === 0) {
					throw new Error('this book has no page count recorded yet');
				}
			} else {
				// Streamed with range requests, so a 158 MB reference book does
				// not have to be downloaded in full before page one appears.
				pdf = await pdfjs.getDocument({
					url: `/api/v1/books/${encodeURIComponent(id)}/file`,
					withCredentials: true
				}).promise;

				pageCount = pdf.numPages;
			}

			// Deliberately NOT awaited. Resolving an outline means following
			// destination references through the PDF, which on the largest book
			// in a real library is over a second - and the reader has the file
			// open and could be showing page one already. The contents button
			// appears when the answer arrives.
			void api
				.readerManifest(id)
				.then((manifest) => {
					toc = manifest.toc ?? [];
				})
				.catch(() => {
					/* a PDF is perfectly readable without a contents list */
				});

			// A citation names the page it came from, and following one should
			// land there rather than at the front of the book. It wins over the
			// saved position: somebody who clicked a reference asked to be here,
			// not where they last stopped.
			const cited = citedPage();
			if (cited && cited <= pageCount) {
				pageNumber = cited;
			} else {
				// Resume. A PDF locator names a page, which is stable because the
				// pages themselves are fixed.
				try {
					const saved = await api.readingProgress(id);
					if (saved.started && saved.locator) {
						const target = pageFromLocator(saved.locator);
						if (target && target <= pageCount) pageNumber = target;
					}
				} catch {
					/* a missing position is not a reason to refuse to open the book */
				}
			}

			loading = false;
			await renderPage();
		} catch (err) {
			error = err instanceof Error ? err.message : String(err);
			loading = false;
			awaitingFirstPage = false;
			slowToOpen = false;
		} finally {
			clearTimeout(explain);
		}
	}

	/**
	 * Pages held in memory, keyed by page number.
	 *
	 * PROMISES rather than documents, so that turning onto a page whose
	 * prefetch is still in flight waits for that request instead of starting a
	 * second one. Flipping fast is exactly when this happens.
	 */
	const loaded = new Map<number, Promise<unknown>>();

	// Which render is current. A render whose generation has been superseded
	// stops instead of painting a page the reader has already left.
	let renderGeneration = 0;
	// The render in flight, awaited before starting another so that two of them
	// never hold the same canvas.
	let previousRender: Promise<unknown> | null = null;

	/**
	 * How many pages to keep: the one being read and one either side.
	 *
	 * Three because that is what makes flipping feel immediate - the next page
	 * is usually already here - without holding a meaningful part of the book
	 * in the browser. Someone taking the whole thing still has to ask for it a
	 * page at a time, which is the point.
	 */
	const keepPages = 1;

	function pageURL(n: number, prefetch: boolean): string {
		const id = encodeURIComponent(page.params.id ?? '');
		return `/api/v1/books/${id}/page/${n}${prefetch ? '?prefetch=1' : ''}`;
	}

	/**
	 * Fetches one page and parses it, or returns the one already in flight.
	 *
	 * `prefetch` is passed to the server so it can tell a page somebody looked
	 * at from one fetched on their behalf. Counting the second as read would
	 * make the audit say a reader saw three pages when they saw one.
	 */
	function pageDocument(n: number, prefetch: boolean): Promise<unknown> {
		const existing = loaded.get(n);
		if (existing) return existing;

		const pending = (async () => {
			const response = await fetch(pageURL(n, prefetch), { credentials: 'same-origin' });
			if (!response.ok) throw new Error(`page ${n} could not be loaded`);
			const bytes = new Uint8Array(await response.arrayBuffer());
			// eslint-disable-next-line @typescript-eslint/no-explicit-any
			return await (lib as any).getDocument({ data: bytes }).promise;
		})();

		loaded.set(n, pending);
		// A failed page must not stay in the map as a permanently rejected
		// promise: the next attempt should be allowed to try again.
		void pending.catch(() => loaded.delete(n));
		return pending;
	}

	/** Drops every page outside the window, freeing what pdf.js holds. */
	function evictAround(centre: number) {
		for (const [n, doc] of loaded) {
			if (Math.abs(n - centre) <= keepPages) continue;
			loaded.delete(n);
			// eslint-disable-next-line @typescript-eslint/no-explicit-any
			void doc.then((d: any) => d?.destroy()).catch(() => {});
		}
	}

	/**
	 * Warms the pages either side of where the reader is.
	 *
	 * Deliberately not awaited: the page being read must not wait for its
	 * neighbours. A failure is ignored because nobody asked for these.
	 */
	function prefetchAround(centre: number) {
		for (const n of [centre + 1, centre - 1]) {
			if (n < 1 || n > pageCount || loaded.has(n)) continue;
			void pageDocument(n, true).catch(() => {});
		}
	}

	/**
	 * The page to paint, from whichever source this book is being read from.
	 *
	 * In per-page mode each page arrives as its own one-page document, so the
	 * page wanted is always page one OF THAT DOCUMENT.
	 */
	async function pageObject() {
		if (!perPage) return pdf ? await pdf.getPage(pageNumber) : null;
		if (!lib) return null;

		const wanted = pageNumber;
		// eslint-disable-next-line @typescript-eslint/no-explicit-any
		const doc = (await pageDocument(wanted, false)) as any;

		// Only warm neighbours if the reader is still on this page: flipping
		// quickly would otherwise queue prefetches for pages already left.
		if (pageNumber === wanted) {
			evictAround(wanted);
			prefetchAround(wanted);
		}
		return await doc.getPage(1);
	}

	// Highlights and notes on this book. The store, the API and the locator
	// format have been format-agnostic since they were written - `pdf:<page>`
	// was always a legal locator - and only the reader was EPUB-only, because
	// a canvas has no text to anchor to.
	let marks = $state<Annotation[]>([]);
	// What was selected, captured WHEN it was selected.
	//
	// Not read back when the button is pressed: pressing a button moves focus
	// and Firefox collapses the selection on the way, so by the time the
	// handler ran there was nothing left to quote and the note silently never
	// appeared. The gesture is over by then anyway - what it selected is a
	// fact, and facts are recorded when they happen.
	let pendingSelection = $state<{
		x: number;
		y: number;
		quote: string;
		occurrence: number;
	} | null>(null);
	let activeNote = $state<Annotation | null>(null);
	let noteDraft = $state('');

	// Importing the book's own highlights.
	//
	// Offered rather than done automatically: it replaces whatever the last
	// import produced, and a thing that rewrites somebody's marks should be
	// something they asked for.
	let importing = $state(false);
	let importNote = $state<string | null>(null);

	async function importOwnMarks() {
		const id = page.params.id;
		if (!id || !pdf || importing) return;
		importing = true;
		importNote = 'Reading this book’s own highlights…';
		try {
			const { readMarks } = await import('$lib/pdfmarks');
			const found = await readMarks(pdf, (n, total) => {
				importNote = `Reading page ${n} of ${total}…`;
			});
			if (found.length === 0) {
				importNote = 'This book carries no highlights of its own.';
				return;
			}
			const result = await api.importMarks(id, found);
			importNote =
				result.imported === result.offered
					? `Imported ${result.imported} highlight${result.imported === 1 ? '' : 's'}.`
					: `Imported ${result.imported} of ${result.offered}; the rest mark pictures rather than words.`;
			await loadMarks(id);
			paintMarks();
		} catch (err) {
			importNote = err instanceof Error ? err.message : String(err);
		} finally {
			importing = false;
		}
	}

	async function loadMarks(id: string) {
		try {
			marks = (await api.annotations(id)).items;
		} catch {
			// Somebody else's book, or a reader who may not annotate. The book
			// is still readable, which is the part that matters.
			marks = [];
		}

		// Paint them, because they may have arrived too late to be painted.
		//
		// This runs alongside opening the book rather than before it, and
		// nothing re-paints when `marks` changes - so whenever the annotations
		// request settled after the first page had rendered, every highlight
		// on that page was invisible until the reader turned away and back.
		// The EPUB reader avoids it by awaiting the marks before the first
		// chapter renders; here the render has usually already happened, so
		// the fix is to paint again once they are in.
		if (textLayer) paintMarks();
	}

	/** The marks that belong to the page on screen. */
	function marksHere(): Annotation[] {
		return marks.filter((m) => pageFromLocator(m.start) === pageNumber);
	}


	/** Where a raw offset falls, as a node and an offset within it. */

	/**
	 * Finds a mark's text in the rendered layer and returns its rectangles.
	 *
	 * Anchored by the WORDS, not by coordinates or character offsets. A PDF's
	 * text can be extracted two ways that disagree - the server reads these
	 * books with a different library than the browser does - and a rectangle
	 * saved at one zoom level means nothing at another. The quoted text is the
	 * one thing that survives both.
	 *
	 * `occurrence` disambiguates a phrase that appears more than once on a
	 * page, which for a short quotation is most of them.
	 */

	/**
	 * Draws the marks for this page as boxes over the canvas.
	 *
	 * Rectangles rather than styled spans: the text layer is pdf.js's and gets
	 * rebuilt on every render, so anything written into it is lost on the next
	 * page turn. Boxes sit in their own layer and are simply redrawn.
	 */
	function paintMarks() {
		if (!textLayer || !highlightLayer) return;
		highlightLayer.replaceChildren();

		const base = highlightLayer.getBoundingClientRect();
		for (const mark of marksHere()) {
			const range = rangeOf(textLayer, mark.selectedText ?? '', occurrenceOf(mark));
			if (!range) continue;
			for (const rect of range.getClientRects()) {
				const box = document.createElement('div');
				box.className = mark.note ? 'mark noted' : 'mark';
				box.style.left = `${rect.left - base.left}px`;
				box.style.top = `${rect.top - base.top}px`;
				box.style.width = `${rect.width}px`;
				box.style.height = `${rect.height}px`;
				box.title = mark.note || mark.selectedText || '';
				box.onclick = () => openNote(mark);
				highlightLayer.append(box);
			}
		}
	}

	/**
	 * Which occurrence of its own text a mark refers to.
	 *
	 * Carried in the locator's character-offset field, which the PDF reader has
	 * never used for anything else: a citation only ever navigates to a page.
	 */
	function occurrenceOf(mark: Annotation): number {
		const parts = mark.start.split(':');
		const n = Number(parts[2]);
		return Number.isFinite(n) && n > 0 ? n : 0;
	}

	function onSelection() {
		if (!textLayer) return;
		const selection = window.getSelection();
		// Only a selection inside the page's own words. Selecting the chrome,
		// or a note being written, is not a highlight.
		const anchor = selection?.anchorNode;
		if (!anchor || !textLayer.contains(anchor)) {
			pendingSelection = null;
			return;
		}
		const quote = selection?.toString().replace(/\s+/g, ' ').trim() ?? '';
		if (!selection || selection.isCollapsed || quote.length < 2) {
			pendingSelection = null;
			return;
		}
		const rect = selection.getRangeAt(0).getBoundingClientRect();
		pendingSelection = {
			x: rect.left + rect.width / 2,
			y: rect.top,
			quote,
			occurrence: occurrenceIn(textLayer, selection, quote)
		};
	}

	async function highlightSelection(withNote: boolean) {
		const id = page.params.id;
		const pending = pendingSelection;
		if (!id || !pending) return;
		const { quote, occurrence } = pending;

		try {
			const created = await api.createAnnotation(id, {
				// Always a highlight. A note with no body is refused by the
				// schema, and there IS no body yet - the editor opens after
				// this returns. Writing one promotes it; see UpdateAnnotation.
				type: 'highlight',
				start: `pdf:${pageNumber}:${occurrence}`,
				end: `pdf:${pageNumber}:${occurrence}`,
				selectedText: quote
			});
			marks = [...marks, created];
			window.getSelection()?.removeAllRanges();
			pendingSelection = null;
			paintMarks();
			if (withNote) {
				activeNote = created;
				noteDraft = '';
			}
		} catch (err) {
			notice = err instanceof Error ? err.message : String(err);
		}
	}

	/**
	 * Counts how many times the selected phrase appears before this one.
	 *
	 * Counted in the same indexed text that rangeOf searches, rather than in
	 * whatever a Range stringifies to. Two ways of reading the page would
	 * disagree about whitespace sooner or later, and a mark would then be
	 * painted over the wrong copy of its own words.
	 */

	function openNote(mark: Annotation) {
		activeNote = mark;
		noteDraft = mark.note ?? '';
	}

	async function saveNote() {
		if (!activeNote) return;
		try {
			const updated = await api.updateAnnotation(activeNote.id, noteDraft);
			marks = marks.map((m) => (m.id === updated.id ? updated : m));
			activeNote = null;
			paintMarks();
		} catch (err) {
			notice = err instanceof Error ? err.message : String(err);
		}
	}

	async function removeMark(mark: Annotation) {
		try {
			await api.deleteAnnotation(mark.id);
			marks = marks.filter((m) => m.id !== mark.id);
			activeNote = null;
			paintMarks();
		} catch (err) {
			notice = err instanceof Error ? err.message : String(err);
		}
	}

	/**
	 * Lays pdf.js's text over the rendered page.
	 *
	 * A canvas holds pixels and nothing else, so until this existed there was
	 * literally nothing on the page to select, and a PDF could not be
	 * highlighted while a reflowable book could. The layer is a stack of
	 * transparent spans positioned exactly over the words they correspond to;
	 * selecting them selects the book.
	 *
	 * Rebuilt on every render rather than transformed, because a page turn
	 * replaces the words and a zoom moves every one of them.
	 */
	async function paintTextLayer(
		p: { getTextContent: () => Promise<unknown> },
		viewport: unknown,
		scale: number,
		generation: number
	) {
		if (!textLayer || !lib) return;
		// Superseded before this even started: the newer render owns the layer.
		if (generation !== renderGeneration) return;

		textLayer.replaceChildren();

		// pdf.js positions and sizes every span with
		// `calc(<n>px * var(--total-scale-factor))`, so without this the whole
		// layer collapses towards the origin at its unscaled size. The marks
		// then paint a line high and a third of the width of their own words,
		// which looks like an anchoring bug and is not one.
		textLayer.style.setProperty('--total-scale-factor', String(scale));

		try {
			// An empty result is the ordinary answer for a scanned page, a cover
			// or a plate, and not a failure: those pages simply cannot be
			// highlighted, which is what a reader would expect of a picture.
			const content = await p.getTextContent();
			if (generation !== renderGeneration) return;

			// eslint-disable-next-line @typescript-eslint/no-explicit-any
			const layer = new (lib as any).TextLayer({
				textContentSource: content,
				container: textLayer,
				viewport
			});
			await layer.render();

			// A superseded render leaves the layer ALONE. Clearing it here is
			// what produced a page whose words could not be selected at all,
			// intermittently: this call would finish after a newer one had
			// already painted, and empty the layer the newer one had just
			// filled. What was left was a container pdf.js had sized, holding
			// nothing - which looks exactly like a page with no text in it.
			if (generation !== renderGeneration) return;
			paintMarks();
		} catch (err) {
			// A page whose text cannot be laid out is still a page somebody can
			// read; it just cannot be highlighted. Said rather than swallowed,
			// because a reader wondering why selection does nothing has no
			// other way to find out - and because a silent catch here cost an
			// afternoon of looking for a bug that was a picture of a page.
			console.warn('this page has no selectable text layer:', err);
		}
	}

	async function renderPage() {
		if (!canvas || !container) return;
		if (!perPage && !pdf) return;

		// Cancel any render still in flight: turning pages quickly would
		// otherwise paint an earlier page over a later one.
		renderTask?.cancel();
		rendering = true;

		// Claim this render. Anything that starts after it supersedes it, and
		// this one must then stop rather than paint over the newer page.
		//
		// Cancelling is not enough on its own. Cancel is synchronous but
		// fetching the page is not, so two renders can both get past the cancel
		// and reach p.render() on the SAME canvas, which pdf.js refuses. That
		// was invisible while every page turn waited on a network round trip,
		// and became reliable the moment prefetching made turns instant.
		const generation = ++renderGeneration;

		// Wait for the cancelled render to actually finish letting go of the
		// canvas. A cancelled task still settles; starting before it does is
		// what produces "cannot use the same canvas".
		await previousRender?.catch(() => {});
		if (generation !== renderGeneration) return;

		try {
			const p = await pageObject();
			if (!p) return;
			if (generation !== renderGeneration) return;
			const unscaled = p.getViewport({ scale: 1 });

			const available = container.clientWidth - 32;
			const availableHeight = container.clientHeight - 32;
			const fitScale =
				prefs.fit === 'width'
					? available / unscaled.width
					: Math.min(available / unscaled.width, availableHeight / unscaled.height);

			// Render at device resolution: a page rendered at CSS pixels and
			// upscaled looks soft, which on a page of body text is the difference
			// between readable and tiring.
			const dpr = window.devicePixelRatio || 1;
			const scale = fitScale * prefs.zoom;
			const viewport = p.getViewport({ scale: scale * dpr });

			canvas.width = viewport.width;
			canvas.height = viewport.height;
			canvas.style.width = `${viewport.width / dpr}px`;
			canvas.style.height = `${viewport.height / dpr}px`;

			const context = canvas.getContext('2d');
			if (!context) return;

			const task = p.render({ canvasContext: context, viewport });
			renderTask = task;
			previousRender = task.promise;
			await task.promise;
			if (generation !== renderGeneration) return;
			renderTask = null;

			// The text layer is laid out in CSS pixels, not device ones: it
			// sits over the canvas at the size the canvas is DISPLAYED, and
			// the canvas is rendered larger than that for sharpness.
			await paintTextLayer(p, p.getViewport({ scale }), scale, generation);
			awaitingFirstPage = false;
			slowToOpen = false;
			void saveProgress();
		} catch (err) {
			// A cancelled render is the expected outcome of a fast page turn, not
			// a failure worth showing anyone.
			if (err && typeof err === 'object' && 'name' in err && err.name === 'RenderingCancelledException') {
				return;
			}
			error = err instanceof Error ? err.message : String(err);
			awaitingFirstPage = false;
			slowToOpen = false;
		} finally {
			rendering = false;
		}
	}

	let saveTimer: ReturnType<typeof setTimeout>;

	/**
	 * Records the page, after a pause.
	 *
	 * The book and the page are both captured when the turn happens, not read
	 * when the timer fires. Read at fire time, a timer surviving a navigation
	 * took the NEW book's id and the OLD book's page number and wrote one onto
	 * the other - so going straight from one PDF to another moved your place in
	 * the second book to wherever you had been in the first.
	 */
	function saveProgress() {
		clearTimeout(saveTimer);
		const id = page.params.id;
		const at = pageNumber;
		const total = pageCount;
		saveTimer = setTimeout(() => void writeProgress(id, at, total), 1000);
	}

	async function writeProgress(id: string | undefined, at: number, total: number) {
		if (!id || total === 0) return;
		try {
			await api.saveProgress(id, `pdf:${at}:0`, at / total);
		} catch {
			/* the next page turn tries again */
		}
	}

	// The last page turn before leaving is worth keeping.
	//
	// The debounce is a second long and nothing flushed it, so turning a page
	// and tapping "← Library" straight after simply lost the turn: come back
	// and you are one page behind where you stopped.
	$effect(() => {
		const id = page.params.id;
		return () => {
			clearTimeout(saveTimer);
			void writeProgress(id, pageNumber, pageCount);
		};
	});

	function goTo(n: number) {
		const target = Math.max(1, Math.min(pageCount, n));
		if (target === pageNumber) return;
		pageNumber = target;
		void renderPage();
	}


	function onKey(event: KeyboardEvent) {
		if (isTyping(event.target)) return;

		switch (event.key) {
			case 'ArrowRight':
			case 'PageDown':
			case ' ':
				event.preventDefault();
				goTo(pageNumber + 1);
				break;
			case 'ArrowLeft':
			case 'PageUp':
				event.preventDefault();
				goTo(pageNumber - 1);
				break;
			case 'Home':
				goTo(1);
				break;
			case 'End':
				goTo(pageCount);
				break;
			case 'Escape':
				showSettings = false;
				chromeVisible = true;
				break;
		}
	}

	function onTap(event: MouseEvent) {
		if (!container) return;

		// A click on something floating over the page is aimed at that thing,
		// not at the page underneath it. Without this, saving a note turned
		// the page out from under the note - the editor sits in the right-hand
		// third, which is the "forward" tap - and opening a mark turned the
		// page away from the mark.
		const target = event.target as Element | null;
		if (target?.closest?.('.selection-actions, .note-editor, .mark')) return;

		const x = event.clientX - container.getBoundingClientRect().left;
		const third = container.clientWidth / 3;
		if (x < third) goTo(pageNumber - 1);
		else if (x > third * 2) goTo(pageNumber + 1);
		else chromeVisible = !chromeVisible;
	}

	$effect(() => {
		loadPrefs();
	});

	$effect(() => {
		const id = page.params.id;
		if (id) {
			void open(id);
			void loadMarks(id);
		}

		// Closing the book frees what pdf.js is holding. Without this, every
		// book opened in a session keeps its window of pages alive.
		return () => {
			for (const [, doc] of loaded) {
				// eslint-disable-next-line @typescript-eslint/no-explicit-any
				void doc.then((d: any) => d?.destroy()).catch(() => {});
			}
			loaded.clear();
			void pdf?.destroy();
		};
	});

	$effect(() => {
		savePrefs();
	});

	// Re-render when the fit, zoom, or viewport changes: all three change how
	// large the page should be drawn.
	$effect(() => {
		void prefs.fit;
		void prefs.zoom;
		if (!container) return;

		const observer = new ResizeObserver(() => void renderPage());
		observer.observe(container);
		void renderPage();
		return () => observer.disconnect();
	});
</script>

<svelte:head><title>Reading · Lexarii</title></svelte:head>
<!-- mouseup on the window, not on the text layer: a selection ends wherever
     the pointer is released, which is regularly outside the words it began
     in, and a listener on the layer would miss exactly the long selections
     somebody most wants to keep. -->
<svelte:window onkeydown={onKey} onmouseup={onSelection} />

<div class="reader" class:invert={prefs.invert}>
	{#if notice}
		<p class="notice" role="status">
			{notice}
			<button onclick={() => (notice = null)} aria-label="Dismiss">×</button>
		</p>
	{/if}

	{#if error}
		<div class="message">
			<p class="error" role="alert">{error}</p>
			<a href="/">← Library</a>
		</div>
	{:else}
		<div
			class="stage"
			bind:this={container}
			onclick={onTap}
			role="presentation"
			aria-busy={loading || rendering}
		>
			{#if loading || awaitingFirstPage}
				<p class="message">
					Opening…
					{#if slowToOpen}
						<span class="why">
							A large book is read once to find its pages. Every page after this
							one is immediate.
						</span>
					{/if}
				</p>
			{/if}
			<!--
				The canvas is the page; the text layer is pdf.js's invisible
				copy of the words, positioned over it. It exists so a PDF can
				be SELECTED - a canvas has no text in it at all, which is why
				this reader could not highlight anything while the reflowable
				one could.
			-->
			<div class="page" class:hidden={loading}>
				<canvas bind:this={canvas}></canvas>
				<!-- Under the text so a mark never swallows the selection. -->
				<div bind:this={highlightLayer} class="highlight-layer" aria-hidden="true"></div>
				<div bind:this={textLayer} class="text-layer"></div>
			</div>

			{#if importNote}
				<p class="import-note" role="status">{importNote}</p>
			{/if}

			{#if pendingSelection}
				<div
					class="selection-actions"
					style="left: {pendingSelection.x}px; top: {pendingSelection.y}px"
				>
					<button onclick={() => void highlightSelection(false)}>Highlight</button>
					<button onclick={() => void highlightSelection(true)}>Note</button>
				</div>
			{/if}

			{#if activeNote}
				<div class="note-editor">
					<p class="quote">“{activeNote.selectedText}”</p>
					<textarea bind:value={noteDraft} placeholder="A note about this passage…"></textarea>
					<div class="note-actions">
						<button onclick={() => void saveNote()}>Save</button>
						<button onclick={() => (activeNote = null)}>Close</button>
						<button
							class="danger"
							onclick={() => activeNote && void removeMark(activeNote)}
						>
							Delete
						</button>
					</div>
				</div>
			{/if}
		</div>

		<div class="chrome" class:hidden={!chromeVisible}>
			<header>
				<a class="control" href="/" aria-label="Back to the library">←</a>
				<span class="label">PDF</span>
				<!-- Same offer as the reflowable reader: the question is about the
				     book, and which format it happens to be held in is not the
				     reader's business. -->
				<a
					class="control ask"
					href="/ask?book={page.params.id}"
					title="Ask about this book"
					aria-label="Ask about this book">Ask</a>
				<!--
					Only offered for a book held whole: reading a PDF's own
					annotations means reading the file, and a book shared to be
					read is served a page at a time and never handed over.

					Gated on `pdf` alone. It used to sit inside the contents
					button's `{#if toc.length > 0}` as well, with indentation
					that hid the nesting - so a PDF with no outline, which is
					most scanned and self-published books, could never import
					the highlights it carries and nothing on screen said why.
				-->
				{#if pdf}
					<button
						class="control"
						onclick={() => void importOwnMarks()}
						disabled={importing}
						title="Import the highlights this PDF carries in its own file"
						aria-label="Import this book's own highlights"
					>
						{importing ? '…' : '⤓'}
					</button>
				{/if}
				{#if toc.length > 0}
					<button class="control" onclick={() => (showToc = !showToc)} aria-label="Contents">☰</button>
				{/if}
				<button class="control" onclick={() => (showSettings = !showSettings)} aria-label="View settings">
					⚙
				</button>
			</header>

			<footer>
				<button class="control" onclick={() => goTo(pageNumber - 1)} aria-label="Previous page">‹</button>
				<input
					class="jump"
					type="number"
					min="1"
					max={pageCount}
					value={pageNumber}
					onchange={(e) => goTo(Number(e.currentTarget.value))}
					aria-label="Go to page"
				/>
				<span class="pages">of {pageCount.toLocaleString()}</span>
				<div class="progress" role="progressbar" aria-valuenow={pageNumber} aria-valuemin={1} aria-valuemax={pageCount}>
					<div class="bar" style="width: {pageCount ? (pageNumber / pageCount) * 100 : 0}%"></div>
				</div>
				<button class="control" onclick={() => goTo(pageNumber + 1)} aria-label="Next page">›</button>
			</footer>
		</div>

		{#if showToc}
			<aside class="panel toc">
				<h2>Contents</h2>
				<ul>
					{#each toc as entry, i (i)}
						<li>
							<button
								disabled={entry.spineIndex < 0}
								onclick={() => {
									showToc = false;
									goTo(entry.spineIndex);
								}}
							>
								<span class="entry-title">{entry.title}</span>
								{#if entry.spineIndex >= 0}<span class="entry-page">{entry.spineIndex}</span>{/if}
							</button>
							{#if entry.children?.length}
								<ul>
									{#each entry.children as child, j (j)}
										<li>
											<button
												disabled={child.spineIndex < 0}
												onclick={() => {
													showToc = false;
													goTo(child.spineIndex);
												}}
											>
												<span class="entry-title">{child.title}</span>
												{#if child.spineIndex >= 0}<span class="entry-page">{child.spineIndex}</span>{/if}
											</button>
										</li>
									{/each}
								</ul>
							{/if}
						</li>
					{/each}
				</ul>
			</aside>
		{/if}

		{#if showSettings}
			<aside class="panel">
				<h2>View</h2>

				<div class="row">
					<span class="row-label">Fit</span>
					<div class="options">
						<button class:active={prefs.fit === 'width'} onclick={() => (prefs.fit = 'width')}>Width</button>
						<button class:active={prefs.fit === 'page'} onclick={() => (prefs.fit = 'page')}>Page</button>
					</div>
				</div>

				<label class="row">
					<span class="row-label">Zoom</span>
					<input type="range" min="0.5" max="3" step="0.1" bind:value={prefs.zoom} />
					<span class="value">{Math.round(prefs.zoom * 100)}%</span>
				</label>

				<label class="row checkbox">
					<input type="checkbox" bind:checked={prefs.invert} />
					<span>Dark mode</span>
				</label>
				<!--
					Inverting a rendered page is an approximation, not a theme: it
					flips photographs and diagrams too. Said plainly rather than
					presented as an equal of the EPUB reader's real themes.
				-->
				<p class="note">Inverts the rendered page. Images and diagrams invert too.</p>

				<button class="reset" onclick={() => (prefs = { ...defaults })}>Reset</button>
			</aside>
		{/if}
	{/if}
</div>

<style>
	/*
	 * The reader owns the whole screen and its own palette, for the same
	 * reason the EPUB reader does: a reading surface is a different object
	 * from an application chrome, and it must not change colour when somebody
	 * switches the library's theme underneath it.
	 *
	 * Written as tokens rather than as nineteen literals scattered through
	 * twelve rules, which is what this was. The same grey appeared four times,
	 * the same hairline three, and the same shadow four - so changing any of
	 * them meant finding all of them, and `make lint`'s token check cannot
	 * help because a literal is not a missing token.
	 *
	 * A fixed dark surround rather than a themed one: these are page IMAGES,
	 * and the only honest way to darken them is the invert filter, which is
	 * offered separately and labelled as what it is.
	 */
	.reader {
		position: fixed;
		inset: 0;
		background: var(--page-surround);
		overflow: hidden;

		--page-surround: #3a3a3c;
		--page-chrome: rgb(28 28 30 / 0.92);
		--page-chrome-solid: rgb(28 28 30 / 0.96);
		--page-ink: #c8c4bd;
		--page-ink-bright: #fff;
		--page-hairline: rgb(255 255 255 / 0.2);
		--page-shadow: rgb(0 0 0 / 0.4);
		--page-danger: #d8756a;
	}

	.stage {
		position: absolute;
		inset: 0;
		display: flex;
		align-items: center;
		justify-content: center;
		overflow: auto;
		cursor: pointer;
		padding: var(--space-4);
	}

	canvas {
		box-shadow: 0 2px 16px var(--page-shadow);
		background: var(--page-ink-bright);
		display: block;
	}

	.page.hidden {
		display: none;
	}

	/* The three layers of a page, in order: pixels, marks, words. The words
	   are last so a mark can never intercept a selection. */
	.page {
		position: relative;
		/* pdf.js measures its spans against a line box of exactly one, and
		   anything else moves every one of them vertically. */
		line-height: 1;
	}

	.highlight-layer,
	.text-layer {
		position: absolute;
		inset: 0;
		overflow: hidden;
	}

	.highlight-layer {
		pointer-events: none;
	}

	/* pdf.js positions these itself, one per run of text, and leaves the SIZE
	   to the stylesheet: each span carries `--font-height` and `--scale-x` and
	   nothing else, expecting a rule like this one to turn them into a font
	   size and a horizontal stretch. Shipping pdf.js's own viewer CSS would
	   bring a great deal else with it, so the two rules that matter are here.

	   Without them every span renders at the inherited 16px and unstretched,
	   which puts the invisible text nowhere near the visible text and makes a
	   highlight land a line high and a third of the width of its own words.
	   The layer is invisible either way, so nothing about the page looks
	   wrong - only the marks do. */
	.text-layer :global(span),
	.text-layer :global(br) {
		position: absolute;
		color: transparent;
		white-space: pre;
		transform-origin: 0 0;
		font-size: calc(var(--font-height, 1em) * var(--total-scale-factor, 1));
		transform: scaleX(var(--scale-x, 1));
		cursor: text;
	}

	.text-layer :global(span::selection) {
		background: color-mix(in srgb, var(--accent) 45%, transparent);
	}

	/* The mark itself. Multiply so the words underneath stay legible through
	   it, which a flat fill over a canvas does not manage. */
	.highlight-layer :global(.mark) {
		position: absolute;
		background: color-mix(in srgb, var(--accent) 38%, transparent);
		mix-blend-mode: multiply;
		border-radius: 2px;
		pointer-events: auto;
		cursor: pointer;
	}

	.highlight-layer :global(.mark.noted) {
		border-bottom: 2px solid var(--accent);
	}

	/* Offered where the selection ended, which is where the reader is looking. */
	/* Above the page, out of the way of the words, and gone when it has
	   nothing to say. */
	.import-note {
		position: fixed;
		top: var(--space-4);
		left: 50%;
		z-index: 5;
		transform: translateX(-50%);
		margin: 0;
		padding: var(--space-2) var(--space-4);
		border-radius: var(--radius-md);
		background: var(--page-chrome);
		color: var(--page-fg);
		font-size: var(--text-sm);
		box-shadow: 0 2px 12px var(--page-shadow);
	}

	.selection-actions {
		position: fixed;
		z-index: 5;
		display: flex;
		gap: var(--space-1);
		transform: translate(-50%, calc(-100% - var(--space-2)));
		padding: var(--space-1);
		border-radius: var(--radius-md);
		background: var(--page-chrome);
		box-shadow: 0 2px 12px var(--page-shadow);
	}

	.selection-actions button,
	.note-actions button {
		min-height: 44px;
		padding: 0 var(--space-3);
		border: 0;
		border-radius: var(--radius-sm);
		background: transparent;
		color: var(--page-fg);
		font: inherit;
		font-size: var(--text-sm);
		cursor: pointer;
	}

	.selection-actions button:hover,
	.note-actions button:hover {
		background: color-mix(in srgb, var(--page-fg) 12%, transparent);
	}

	.note-editor {
		position: fixed;
		right: var(--space-4);
		bottom: var(--space-4);
		z-index: 5;
		width: min(28rem, calc(100vw - 2 * var(--space-4)));
		padding: var(--space-3);
		border-radius: var(--radius-md);
		background: var(--page-chrome);
		box-shadow: 0 2px 16px var(--page-shadow);
	}

	.note-editor .quote {
		margin: 0 0 var(--space-2);
		color: var(--page-muted);
		font-size: var(--text-sm);
		font-style: italic;
	}

	.note-editor textarea {
		width: 100%;
		min-height: 6rem;
		padding: var(--space-2);
		border: 1px solid var(--page-muted);
		border-radius: var(--radius-sm);
		background: var(--page-bg);
		color: var(--page-fg);
		font: inherit;
		resize: vertical;
	}

	.note-actions {
		display: flex;
		gap: var(--space-1);
		justify-content: flex-end;
		margin-top: var(--space-2);
	}

	.note-actions .danger {
		color: var(--danger);
	}

	/* An approximation of a dark theme, and labelled as one in the UI. */
	.reader.invert canvas {
		filter: invert(1) hue-rotate(180deg);
	}

	.chrome {
		position: absolute;
		inset: 0;
		pointer-events: none;
		transition: opacity var(--duration-fast) var(--ease-out);
	}

	.chrome.hidden {
		opacity: 0;
	}

	.chrome header,
	.chrome footer {
		position: absolute;
		left: 0;
		right: 0;
		display: flex;
		align-items: center;
		gap: var(--space-3);
		padding: var(--space-2) var(--space-4);
		background: var(--page-chrome);
		backdrop-filter: blur(8px);
		pointer-events: auto;
		color: var(--page-ink);
		font-size: var(--text-sm);
	}

	.chrome header {
		top: 0;
		padding-top: max(var(--space-2), env(safe-area-inset-top));
	}

	.chrome footer {
		bottom: 0;
		padding-bottom: max(var(--space-2), env(safe-area-inset-bottom));
	}

	.label {
		flex: 1;
		text-transform: uppercase;
		letter-spacing: 0.06em;
		font-size: var(--text-xs);
	}

	.control {
		display: inline-flex;
		align-items: center;
		justify-content: center;
		min-width: 44px;
		min-height: 44px;
		border: 0;
		background: none;
		color: inherit;
		font: inherit;
		font-size: var(--text-base);
		cursor: pointer;
		text-decoration: none;
	}

	.why {
		display: block;
		max-width: 28rem;
		margin-top: var(--space-2);
		font-size: var(--text-sm);
		opacity: 0.75;
	}

	.control.ask {
		font-size: var(--text-sm);
	}

	.jump {
		width: 4.5rem;
		min-height: 44px;
		padding: 0 var(--space-2);
		border: 1px solid var(--page-hairline);
		border-radius: var(--radius-sm);
		background: none;
		color: inherit;
		font: inherit;
		text-align: center;
	}

	.pages {
		font-variant-numeric: tabular-nums;
		white-space: nowrap;
	}

	.progress {
		flex: 1;
		height: 2px;
		background: var(--page-hairline);
		border-radius: 1px;
	}

	.bar {
		height: 100%;
		background: var(--page-ink);
		border-radius: 1px;
	}

	.toc ul {
		list-style: none;
		margin: 0;
		padding: 0;
	}

	/* Nested entries are indented rather than styled differently: the hierarchy
	   is the information, and a second typeface would only compete with it. */
	.toc ul ul {
		margin-left: var(--space-4);
	}

	.toc button {
		display: flex;
		align-items: baseline;
		gap: var(--space-3);
		width: 100%;
		min-height: 44px;
		padding: var(--space-2) 0;
		border: 0;
		background: transparent;
		color: inherit;
		font: inherit;
		text-align: left;
		cursor: pointer;
	}

	.toc button:hover:not([disabled]) {
		color: var(--page-ink-bright);
	}

	/* An entry whose destination did not resolve is shown, not hidden. It is
	   still the book's own structure, and hiding it would silently misrepresent
	   the contents. It simply cannot be followed. */
	.toc button[disabled] {
		opacity: 0.45;
		cursor: default;
	}

	.entry-title {
		flex: 1;
		min-width: 0;
	}

	.entry-page {
		flex-shrink: 0;
		font-variant-numeric: tabular-nums;
		font-size: var(--text-sm);
		opacity: 0.6;
	}

	.panel {
		position: absolute;
		top: 0;
		right: 0;
		bottom: 0;
		width: min(20rem, 90vw);
		padding: var(--space-6) var(--space-4);
		padding-top: max(var(--space-6), env(safe-area-inset-top));
		background: var(--page-chrome-solid);
		backdrop-filter: blur(12px);
		color: var(--page-ink);
		overflow-y: auto;
		z-index: 2;
	}

	.panel h2 {
		font-size: var(--text-sm);
		text-transform: uppercase;
		letter-spacing: 0.06em;
		margin: 0 0 var(--space-4);
		opacity: 0.7;
	}

	.row {
		display: flex;
		align-items: center;
		gap: var(--space-3);
		min-height: 44px;
		margin-bottom: var(--space-3);
		font-size: var(--text-sm);
	}

	.row-label {
		min-width: 4rem;
		opacity: 0.7;
	}

	.value {
		min-width: 3.5rem;
		text-align: right;
		font-variant-numeric: tabular-nums;
		opacity: 0.7;
	}

	.options {
		display: flex;
		gap: var(--space-2);
	}

	.options button {
		min-height: 44px;
		padding: 0 var(--space-3);
		border: 1px solid var(--page-hairline);
		border-radius: var(--radius-sm);
		background: none;
		color: inherit;
		font: inherit;
		font-size: var(--text-sm);
		cursor: pointer;
	}

	.options button.active {
		border-color: var(--page-ink);
	}

	.note {
		font-size: var(--text-xs);
		opacity: 0.6;
		margin: 0 0 var(--space-4);
		max-width: 22rem;
	}

	.reset {
		min-height: 44px;
		width: 100%;
		border: 1px solid var(--page-hairline);
		border-radius: var(--radius-sm);
		background: none;
		color: inherit;
		font: inherit;
		font-size: var(--text-sm);
		cursor: pointer;
	}

	input[type='range'] {
		flex: 1;
	}

	.checkbox {
		gap: var(--space-2);
	}

	.message {
		display: flex;
		flex-direction: column;
		gap: var(--space-4);
		align-items: center;
		justify-content: center;
		color: var(--page-ink);
		text-align: center;
	}

	.error {
		color: var(--page-danger);
		max-width: var(--measure);
	}

	.notice {
		position: fixed;
		left: 50%;
		bottom: var(--space-6);
		transform: translateX(-50%);
		z-index: 20;
		display: flex;
		align-items: center;
		gap: var(--space-3);
		max-width: min(90vw, 32rem);
		margin: 0;
		padding: var(--space-3) var(--space-4);
		border: 1px solid var(--border);
		border-radius: var(--radius-md);
		background: var(--bg-raised, var(--bg));
		color: var(--fg);
		font-size: var(--text-sm);
	}

	.notice button {
		min-width: 44px;
		min-height: 44px;
		border: none;
		background: none;
		color: var(--fg-muted);
		font-size: var(--text-lg);
		cursor: pointer;
	}
</style>

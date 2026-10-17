<!-- SPDX-License-Identifier: AGPL-3.0-or-later -->
<script lang="ts">
	import { tick } from 'svelte';
	import { page } from '$app/state';
	import { goto } from '$app/navigation';
	import * as api from '$api/client';
	import { isTyping } from '$lib/reader/keys';
	import { currentEntry, flattenToc, headingBefore } from '$lib/reader/toc';
	import type { ReaderManifest, TOCEntry } from '$api/client';
	import {
		anchorFromSelection,
		elementAtPath,
		elementPath,
		firstVisibleElement,
		formatEpubLocator,
		paintHighlight,
		parseLocator,
		pendingImages,
		rangeFromAnchor,
		unpaintHighlights,
		whenLoaded,
		type ParsedLocator
	} from '$lib/reader/locator';
	import type { Annotation } from '$api/client';

	let manifest = $state<ReaderManifest | null>(null);
	// The book being read. The reader could not name it before: nothing on this
	// path carried a title, so the chrome could only count spine items.
	let bookTitle = $state('');
	let workId = $state('');
	let error = $state<string | null>(null);

	// A failure that does not cost the reader their book.
	//
	// `error` replaces the whole reading surface, which is right when the book
	// could not be loaded and wrong for everything else. A highlight that
	// failed to save on a flaky connection used to tear the rendered chapter
	// out of the DOM and leave a "← Library" link in its place - and, because
	// nothing ever cleared it, it stayed torn out across a client-side
	// navigation to a different book.
	let notice = $state<string | null>(null);
	let spineIndex = $state(0);
	let html = $state('');
	let loading = $state(true);

	// Pagination state. `page` counts columns within the current spine item.
	let pageIndex = $state(0);
	let pageCount = $state(1);

	let viewport = $state<HTMLElement | null>(null);
	let flow = $state<HTMLElement | null>(null);

	/**
	 * The path of the paragraph at the top of the page the reader last settled
	 * on. A plain variable, not state: nothing renders it.
	 *
	 * A relayout (a rotation, a resized window, a new font size) cannot work it
	 * out afterwards. By the time the observer or the effect runs, the text has
	 * already reflowed at the new size under the old page number, and asking
	 * then names a paragraph pages away. So it is recorded when the reader
	 * arrives on a page, and a relayout puts that paragraph back on screen.
	 *
	 * The same rule as `here` in the Android reader's reader.js.
	 */
	let here: string | null = null;

	let marks = $state<Annotation[]>([]);
	let pendingSelection = $state<{ x: number; y: number } | null>(null);

	/**
	 * Where the reader had got to, when that is ahead of where they are now.
	 *
	 * Reopening restores the CURRENT position, so someone who turned back to an
	 * earlier chapter lands there - correctly. Without this they would have no
	 * way back to the furthest point except by turning pages, and the furthest
	 * position the server carefully keeps would be invisible.
	 */
	let furthestHint = $state<{ spineIndex: number; path: string; percent: number } | null>(null);
	let activeNote = $state<Annotation | null>(null);
	let noteDraft = $state('');

	let chromeVisible = $state(true);
	let showSettings = $state(false);
	let showToc = $state(false);

	// Typography. Persisted per user rather than per book: someone who needs
	// larger text needs it in every book.
	type Prefs = {
		theme: 'light' | 'sepia' | 'dark' | 'black';
		fontFamily: 'serif' | 'sans';
		fontSize: number;
		lineHeight: number;
		measure: number;
		justify: boolean;
	};

	const defaults: Prefs = {
		theme: 'light',
		fontFamily: 'serif',
		fontSize: 19,
		lineHeight: 1.6,
		measure: 34,
		justify: false
	};

	let prefs = $state<Prefs>({ ...defaults });

	function loadPrefs() {
		try {
			const raw = localStorage.getItem('lexarii.reader');
			if (raw) prefs = { ...defaults, ...JSON.parse(raw) };
		} catch {
			/* private browsing, or cleared storage; defaults are fine */
		}
	}

	function savePrefs() {
		try {
			localStorage.setItem('lexarii.reader', JSON.stringify(prefs));
		} catch {
			/* not worth interrupting reading over */
		}
	}

	/** The position to land on once the chapter has been laid out. */
	let pendingPath = $state<string | null>(null);
	// An element id to land on, from a link inside the book. Paths address a
	// position the reader saved; fragments address one the book named.
	let pendingFragment: string | null = null;
	// Whether pendingPath came from a citation rather than from resuming. Only a
	// citation marks the paragraph it landed on.
	let pendingWasCited = false;

	/**
	 * The passage a citation asked for, if this was opened from one.
	 *
	 * Read once, at load. Nothing clears it from the URL: leaving it there
	 * means the link can be shared or reloaded and still arrive in the right
	 * place, which is what a reference is for.
	 */
	function citedLocator(): ParsedLocator | null {
		const at = page.url.searchParams.get('at');
		return at ? parseLocator(at) : null;
	}

	/**
	 * The chapter a link inside the book asked for, if this was opened from one.
	 *
	 * Read here as well as in the effect below, because on a COLD load the
	 * effect cannot act: it runs before the manifest exists, and loadSpine
	 * needs the manifest to know how many chapters there are. Handled only
	 * there, a link that was copied, shared, or reloaded would silently land
	 * wherever the reader last stopped - which is the one thing a reference
	 * must not do.
	 */
	function requestedSpine(): { index: number; fragment: string | null } | null {
		const raw = page.url.searchParams.get('spine');
		if (raw === null) return null;
		const index = Number(raw);
		if (!Number.isInteger(index) || index < 0) return null;
		return { index, fragment: page.url.searchParams.get('frag') };
	}

	async function loadManifest(id: string) {
		// A previous book's failure must not be showing while this one loads.
		error = null;
		notice = null;
		try {
			// Which surface? A reflowable EPUB and a fixed-page PDF are different
			// readers. Asking first avoids fetching an EPUB manifest for a PDF and
			// reporting a misleading error when it fails.
			const info = await api.bookInfo(id);
			bookTitle = info.title;
			workId = info.workId;
			if (!info.reflowable) {
				// Carry the query string across. A citation into a PDF that
				// arrives here - a shared link, a bookmark - would otherwise be
				// redirected to the front of the book, losing the one thing that
				// made it a reference.
				await goto(`/read/${id}/pdf${page.url.search}`, { replaceState: true });
				return;
			}

			manifest = await api.readerManifest(id);

			// A citation names the passage it came from, and following one should
			// land on that passage rather than at the top of the book. It wins
			// over the saved position: somebody who clicked a reference asked to
			// be here, not where they last stopped.
			let startIndex = 0;
			const cited = citedLocator();

			const asked = requestedSpine();

			if (cited) {
				startIndex = cited.spineIndex;
				pendingPath = cited.path;
				pendingWasCited = true;
			} else if (asked) {
				// A link followed into this book. Claimed here so the effect
				// below does not load the same chapter a second time.
				startIndex = asked.index;
				pendingFragment = asked.fragment;
				handled = page.url.search;
			} else {
				// Resume where the reader left off. A position saved on another
				// device at another font size resolves here too, because it names
				// a paragraph rather than a page.
				try {
					const saved = await api.readingProgress(id);
					if (saved.started && saved.locator) {
						const parsed = parseLocator(saved.locator);
						if (parsed) {
							startIndex = parsed.spineIndex;
							pendingPath = parsed.path;

							const furthest = saved.furthestLocator
								? parseLocator(saved.furthestLocator)
								: null;
							if (furthest && aheadOf(furthest, parsed)) {
								furthestHint = {
									spineIndex: furthest.spineIndex,
									path: furthest.path,
									percent: Math.round((saved.furthestPercent ?? 0) * 100)
								};
							}
						}
					}
				} catch {
					/* a missing position is not a reason to refuse to open the book */
				}
			}

			// Load marks before the first chapter renders, so highlights appear
			// with the text rather than flashing in after it.
			try {
				marks = (await api.annotations(id)).items;
			} catch {
				/* a book is still readable without its highlights */
			}

			// Clamped, because startIndex can come from a URL somebody typed or
			// from a locator saved against a different edition. loadSpine
			// refuses an out-of-range index and returns without clearing
			// `loading`, which would leave the book spinning forever.
			await loadSpine(Math.min(Math.max(startIndex, 0), manifest.spine.length - 1));
		} catch (err) {
			error = err instanceof Error ? err.message : String(err);
			loading = false;
		}
	}

	/**
	 * Waits until new content is in the DOM and laid out.
	 *
	 * tick() is what guarantees the DOM has actually been updated. The animation
	 * frame only lets layout settle before measuring, so it is RACED with a
	 * timer: a page that is not painting - a background tab, an occluded or
	 * minimised window, a phone that has throttled the page - never fires
	 * requestAnimationFrame at all. Waiting on it unconditionally leaves the
	 * chapter rendered but never measured, never painted, and never saved, and
	 * the reader comes back to a half-loaded book.
	 */
	async function settled(): Promise<void> {
		await tick();
		await Promise.race([
			new Promise((r) => requestAnimationFrame(() => r(null))),
			new Promise((r) => setTimeout(r, 50))
		]);
	}

	/**
	 * Guards against overlapping chapter loads.
	 *
	 * Turning pages quickly at a chapter boundary starts a second load before
	 * the first has answered, and responses do not have to come back in order.
	 * Without this, a stale response wins the race and leaves `html` showing one
	 * chapter while `spineIndex` says another - which then paints the wrong
	 * annotations and saves a reading position in a chapter the reader never saw.
	 */
	let loadSeq = 0;
	let inFlight: AbortController | null = null;

	/** How long a chapter waits for the pictures above its target. */
	const PICTURE_WAIT_MS = 3000;

	async function loadSpine(index: number, toEnd = false) {
		const id = page.params.id;
		if (!id || !manifest) return;
		if (index < 0 || index >= manifest.spine.length) return;

		const seq = ++loadSeq;
		inFlight?.abort();
		const controller = new AbortController();
		inFlight = controller;

		loading = true;
		try {
			const content = await api.spineContent(id, index, controller.signal);
			// A newer load started while this one was in the air: its result is the
			// one the reader asked for, so this one is discarded whole.
			if (seq !== loadSeq) return;
			spineIndex = index;
			html = content.html;
			// The previous chapter's paragraph means nothing in this one, and a
			// relayout that ran before this load lands would otherwise go
			// looking for it here.
			here = null;
			// Wait for the new content to lay out before measuring columns.
			await settled();
			if (seq !== loadSeq || !flow) return;

			// What the reader was sent to, when it was something in particular.
			// Found by structure, before anything is measured: no picture can
			// change which element a fragment or a path names. It becomes
			// `here` rather than the first block on its page, so a relayout
			// keeps the cited paragraph on screen and not merely the one above
			// it.
			let landed: Element | null = null;
			let wasCited = false;
			if (pendingFragment) {
				const wanted = pendingFragment;
				pendingFragment = null;
				// CSS.escape because a book's ids are its own business: they
				// contain dots, colons and digits-first, none of which are
				// valid in a bare selector. One that is not there - a reference
				// into a document that no longer contains it - opens the
				// chapter, which is still the right chapter.
				landed = flow.querySelector('#' + CSS.escape(wanted));
			} else if (pendingPath) {
				// The book may have changed under the position - a re-ingest, a
				// different edition. Then this is null and the chapter opens at
				// its start: honest, where guessing a page would not be.
				landed = elementAtPath(pendingPath, flow);
				wasCited = pendingWasCited;
				pendingPath = null;
				pendingWasCited = false;
			}

			// The pictures that can still move it. Measured before they
			// arrive, a saved position resolved to a page several before the
			// one the reader left - see pendingImages. The start of a chapter
			// is page one whatever arrives, so opening there waits for nothing.
			const waiting = landed
				? pendingImages(flow, landed)
				: toEnd
					? pendingImages(flow, null)
					: [];
			if (waiting.length > 0) {
				await whenLoaded(waiting, PICTURE_WAIT_MS);
				if (seq !== loadSeq || !flow) return;
			}
			measure();

			if (landed) {
				// Resolve the paragraph to whichever column it now falls in.
				pageIndex = pageOf(landed);
				// Say WHICH paragraph was cited. Landing on the right page of
				// a wall of text still leaves somebody hunting for the
				// sentence that was quoted at them. It fades: a permanent
				// mark would be mistaken for one of their own highlights.
				if (wasCited) {
					const target = landed;
					target.classList.add('cited');
					setTimeout(() => target.classList.remove('cited'), 2600);
				}
			} else {
				pageIndex = toEnd ? Math.max(0, pageCount - 1) : 0;
			}
			applyOffset(false);
			paintMarks();
			settledOn();
			if (landed && flow && flow.contains(landed)) here = elementPath(landed, flow) || here;
		} catch (err) {
			// A superseded load is not a failure, and must never reach the reader as
			// an error message.
			if (seq !== loadSeq || (err instanceof DOMException && err.name === 'AbortError')) return;
			error = err instanceof Error ? err.message : String(err);
		} finally {
			if (seq === loadSeq) loading = false;
		}
	}

	/**
	 * The gap between columns, derived from the viewport's own padding.
	 *
	 * It cannot be an arbitrary constant. `overflow: hidden` clips at the
	 * viewport's BORDER box, which includes its padding, while a column is only
	 * as wide as the content box. A gap narrower than the padding therefore
	 * leaves the next column peeking through the margin - visibly, as a sliver
	 * of the following page beside the one being read.
	 *
	 * Twice the padding puts the next column a full margin beyond the visible
	 * edge, whatever measure the reader has chosen.
	 */
	function columnGap(): number {
		if (!viewport) return 64;
		const padding = parseFloat(getComputedStyle(viewport).paddingLeft) || 32;
		return Math.max(64, padding * 2);
	}

	/**
	 * Lays the chapter out as columns and measures how many there are.
	 *
	 * CSS multi-column is the only approach that reflows real book HTML into
	 * pages without measuring every glyph in JavaScript: the browser does the
	 * line breaking, and the column count falls out of the scroll width.
	 *
	 * The column width is set here rather than in CSS because the advance
	 * distance must match it EXACTLY. Expressing the width as a calc() in CSS
	 * and the advance as clientWidth in JS looks equivalent and is not -
	 * clientWidth includes padding - and the columns then drift a little further
	 * out of alignment with every page turn. The width is pageWidth() for the
	 * same reason.
	 */
	function measure() {
		if (!flow) return;
		const width = pageWidth();
		if (width <= 0) return;

		const gap = columnGap();
		flow.style.columnWidth = `${width}px`;
		flow.style.columnGap = `${gap}px`;

		// scrollWidth counts n columns and n-1 gaps.
		pageCount = Math.max(1, Math.round((flow.scrollWidth + gap) / (width + gap)));
		if (pageIndex > pageCount - 1) pageIndex = pageCount - 1;
	}

	/**
	 * Moves the column strip. Transform only: never triggers layout.
	 *
	 * `animate` is false for everything that is not a page turn. The transition
	 * exists to make ONE page turn read as a turn; a citation four columns away,
	 * a resumed position, and a re-measure after the font changed are jumps, and
	 * sliding the text through every column in between is both wrong to watch
	 * and slow to paint - the strip is several thousand pixels wide, so the
	 * browser spends seconds compositing columns nobody asked to see.
	 *
	 * Measured on a citation four columns in: the quoted paragraph reached the
	 * screen after 2.9 seconds with the slide and 109ms without it - and the
	 * highlight that says WHICH paragraph was quoted fades after 2.6, so it was
	 * expiring while the reader was still sliding towards it.
	 */
	function applyOffset(animate = true) {
		if (!flow) return;
		const advance = pageWidth() + columnGap();
		const offset = `translateX(${-pageIndex * advance}px)`;
		if (animate) {
			flow.style.transform = offset;
			placeChapter();
			return;
		}
		// Suppressed around the write rather than removed: the next page turn
		// must still animate. Reading a layout property in between is what
		// forces the jump to be committed before transitions come back.
		const previous = flow.style.transition;
		flow.style.transition = 'none';
		flow.style.transform = offset;
		void flow.offsetWidth;
		flow.style.transition = previous;
		placeChapter();
	}

	/**
	 * The width of one page of text, unrounded.
	 *
	 * clientWidth is rounded to a whole pixel and the columns are laid out at
	 * the real width, which is fractional under browser zoom and on many
	 * phones. The difference is added again on every page: a few pages in, the
	 * strip sits pixels off and firstVisibleElement's one-pixel tolerance finds
	 * no paragraph on the page, so nothing is saved. Measured in the Android
	 * reader, which shares this arithmetic; see pageWidth() in its reader.js.
	 * The strip is only ever translated, which leaves the width alone.
	 */
	function pageWidth(): number {
		return flow ? flow.getBoundingClientRect().width : 0;
	}

	/** The page the element starts on, at the current layout. */
	function pageOf(element: Element): number {
		if (!flow) return 0;
		const advance = pageWidth() + columnGap();
		const left = element.getBoundingClientRect().left - flow.getBoundingClientRect().left;
		return Math.max(0, Math.min(pageCount - 1, Math.round(left / advance)));
	}

	/** The path of the first block on the page on screen, or null on an empty page. */
	function visiblePath(): string | null {
		if (!flow) return null;
		const target = firstVisibleElement(flow, pageIndex * (pageWidth() + columnGap()), pageWidth());
		return target ? elementPath(target, flow) || null : null;
	}

	/**
	 * The reader has arrived on a page by their own doing: a turn, a jump, a
	 * chapter opening. Remembers the paragraph there and saves the position.
	 */
	function settledOn() {
		here = visiblePath();
		scheduleSave();
	}

	/**
	 * Lays the chapter out again after the space or the type changed, and puts
	 * the paragraph the reader was on back on screen.
	 *
	 * `here` is deliberately NOT updated from the page this lands on. Most
	 * paragraphs start part-way down a page, so the first block on the new page
	 * is usually an earlier one; adopting it would step back a paragraph on
	 * every change, and dragging the font-size slider would walk the reader
	 * backwards through the chapter.
	 */
	function relayout() {
		if (!flow) return;
		measure();
		const target = here ? elementAtPath(here, flow) : null;
		if (target) pageIndex = pageOf(target);
		applyOffset(false);
		// The same paragraph, but the page it is on - and so the percentage -
		// has moved.
		if (target) scheduleSave();
	}

	/**
	 * A picture arrived after the chapter was laid out - slower than
	 * PICTURE_WAIT_MS, below where the reader landed, or anywhere in a chapter
	 * opened at its start - and pushed everything after it along. Laid out
	 * again around the paragraph being read, once a burst of arrivals has
	 * settled. One that failed changes the layout too: its alt text takes the
	 * space. Neither event bubbles, so the flow listens on the way down; the
	 * Android reader does the same.
	 */
	let pictureTimer: ReturnType<typeof setTimeout>;
	function pictureArrived(event: Event) {
		if (loading || !(event.target instanceof HTMLImageElement)) return;
		clearTimeout(pictureTimer);
		pictureTimer = setTimeout(relayout, 60);
	}

	/**
	 * Records the position, debounced.
	 *
	 * Written optimistically and reconciled with what comes back: the server
	 * resolves conflicts to the FURTHEST position, so a phone that read ahead
	 * wins over a laptop left open on page one. Reading is never blocked on a
	 * save succeeding.
	 */
	let saveTimer: ReturnType<typeof setTimeout>;
	function scheduleSave() {
		clearTimeout(saveTimer);
		saveTimer = setTimeout(() => void saveNow(), 1200);
	}

	/**
	 * Saves the reading position.
	 *
	 * The book is passed in rather than read from the route, because the one
	 * caller that matters runs while the route is CHANGING: the cleanup that
	 * flushes the last page turn fires as the reader navigates away, and by
	 * then `page.params.id` is already the book being opened. Reading it there
	 * writes the page you left onto the book you just opened.
	 */
	async function saveNow(bookID?: string) {
		const id = bookID ?? page.params.id;
		if (!id || !flow || loading) return;

		const target = firstVisibleElement(flow, pageIndex * (pageWidth() + columnGap()), pageWidth());
		if (!target) return;

		const path = elementPath(target, flow);
		if (!path) return;

		const overall =
			manifest && manifest.spine.length > 0
				? (spineIndex + (pageCount > 1 ? pageIndex / (pageCount - 1) : 0)) / manifest.spine.length
				: undefined;

		try {
			await api.saveProgress(id, formatEpubLocator(spineIndex, path), overall);
		} catch {
			/* offline, or the server restarted; the next turn tries again */
		}
	}

	/**
	 * Draws the highlights belonging to this chapter.
	 *
	 * Anchors that no longer resolve are REPORTED, not dropped. A note the
	 * reader wrote is their work: it is kept, flagged, and shown with the text
	 * it was made against, because silently losing it is the one outcome that
	 * cannot be undone.
	 */
	function paintMarks() {
		if (!flow) return;

		// Start from clean text every time. This runs on page turns and after any
		// change to the set of marks, so it has to be repeatable.
		unpaintHighlights(flow);

		const orphans: string[] = [];
		// Marks that were flagged but resolve again. Without reporting these, a
		// single bad render leaves a note flagged as orphaned for the rest of its
		// life, even though the words it points at are right there.
		const recovered: string[] = [];
		for (const mark of marks) {
			const start = parseLocator(mark.start);
			const end = parseLocator(mark.end);
			if (!start || !end || start.spineIndex !== spineIndex) continue;

			const range = rangeFromAnchor(
				{
					startPath: start.path,
					startOffset: start.charOffset,
					endPath: end.path,
					endOffset: end.charOffset,
					text: mark.selectedText ?? ''
				},
				flow
			);

			if (!range || !paintHighlight(range, mark.id, mark.colour ?? '')) {
				if (!mark.orphaned) orphans.push(mark.id);
			} else if (mark.orphaned) {
				recovered.push(mark.id);
			}
		}

		if (orphans.length > 0 || recovered.length > 0) {
			const id = page.params.id;
			if (id) {
				void api.reportOrphans(id, orphans, recovered).catch(() => {});
				marks = marks.map((m) => {
					if (orphans.includes(m.id)) return { ...m, orphaned: true };
					if (recovered.includes(m.id)) return { ...m, orphaned: false };
					return m;
				});
			}
		}

		// Column geometry changes when marks are inserted, because splitting text
		// nodes can change where lines break.
		measure();
		applyOffset(false);
	}

	/** Offers to highlight whatever the reader has just selected. */
	function onSelection() {
		if (!flow) return;
		const selection = window.getSelection();
		if (!selection) {
			pendingSelection = null;
			return;
		}
		const anchor = anchorFromSelection(selection, flow);
		if (!anchor) {
			pendingSelection = null;
			return;
		}
		const rect = selection.getRangeAt(0).getBoundingClientRect();
		pendingSelection = { x: rect.left + rect.width / 2, y: rect.top };
	}

	async function highlightSelection(withNote: boolean) {
		const id = page.params.id;
		if (!id || !flow) return;
		const selection = window.getSelection();
		if (!selection) return;

		const anchor = anchorFromSelection(selection, flow);
		if (!anchor) return;

		try {
			const created = await api.createAnnotation(id, {
				// Always a highlight. A note with no body is refused by the
				// schema, and there IS no body yet - the editor opens after
				// this returns. Writing one promotes it; see UpdateAnnotation.
				type: 'highlight',
				start: formatEpubLocator(spineIndex, anchor.startPath, anchor.startOffset),
				end: formatEpubLocator(spineIndex, anchor.endPath, anchor.endOffset),
				selectedText: anchor.text
			});
			marks = [...marks, created];
			selection.removeAllRanges();
			pendingSelection = null;

			// Repaint the whole set rather than painting the new mark alone, so what
			// is on screen is exactly what a reload would produce.
			paintMarks();

			if (withNote) {
				activeNote = created;
				noteDraft = '';
			}
		} catch (err) {
			notice = err instanceof Error ? err.message : String(err);
		}
	}

	async function saveNote() {
		if (!activeNote) return;
		try {
			const updated = await api.updateAnnotation(activeNote.id, noteDraft);
			marks = marks.map((m) => (m.id === updated.id ? updated : m));
			activeNote = null;
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

	/** Opens the note attached to a highlight the reader tapped. */
	function markAt(target: EventTarget | null): Annotation | null {
		const el = target instanceof Element ? target.closest('mark.sm-highlight') : null;
		const id = el instanceof HTMLElement ? el.dataset.annotation : undefined;
		return id ? (marks.find((m) => m.id === id) ?? null) : null;
	}

	/** Reading order between two parsed locators in the same book. */
	function aheadOf(a: ParsedLocator, b: ParsedLocator): boolean {
		if (a.spineIndex !== b.spineIndex) return a.spineIndex > b.spineIndex;
		if (a.path !== b.path) return a.path > b.path;
		return a.charOffset > b.charOffset;
	}

	function jumpToFurthest() {
		const target = furthestHint;
		if (!target) return;
		furthestHint = null;
		pendingPath = target.path;
		void loadSpine(target.spineIndex);
	}

	function nextPage() {
		if (pageIndex < pageCount - 1) {
			pageIndex++;
			applyOffset();
			paintMarks();
			settledOn();
		} else if (manifest && spineIndex < manifest.spine.length - 1) {
			void loadSpine(spineIndex + 1);
		}
	}

	function prevPage() {
		if (pageIndex > 0) {
			pageIndex--;
			applyOffset();
			paintMarks();
			settledOn();
		} else if (spineIndex > 0) {
			// Entering the previous chapter from the front means landing on its
			// last page, which is what turning back a page actually means.
			void loadSpine(spineIndex - 1, true);
		}
	}


	function onKey(event: KeyboardEvent) {
		if (isTyping(event.target)) return;

		switch (event.key) {
			case 'ArrowRight':
			case 'PageDown':
			case ' ':
				event.preventDefault();
				nextPage();
				break;
			case 'ArrowLeft':
			case 'PageUp':
				event.preventDefault();
				prevPage();
				break;
			case 'Home':
				pageIndex = 0;
				applyOffset(false);
				settledOn();
				break;
			case 'End':
				pageIndex = pageCount - 1;
				applyOffset(false);
				settledOn();
				break;
			case 'Escape':
				showSettings = false;
				showToc = false;
				chromeVisible = true;
				break;
			case 't':
				showToc = !showToc;
				break;
		}
	}

	/**
	 * Whether a selection was showing when this gesture began.
	 *
	 * A click arrives after every mouse selection - Chrome fires one on the
	 * common ancestor of where the drag started and ended - and after the
	 * mouseup that offered Highlight. Read then, "a selection is live" is true
	 * of the gesture that MADE it, and treating that as a tap to dismiss
	 * cleared every mouse selection the instant it was made, so the Highlight
	 * button never stayed up. What decides is whether there was one before
	 * the button went down; the Android reader's page draws the same line.
	 */
	let selectedAtDown = false;
	function onPointerDown() {
		const selection = window.getSelection();
		selectedAtDown = !!selection && !selection.isCollapsed;
	}

	/** Edges turn pages; the centre restores the chrome. The text is the interface. */
	function onTap(event: MouseEvent) {
		if (!viewport) return;

		// Made by this gesture - a drag, or a double or triple click, whose
		// earlier clicks made the selection this one extends: somebody reading
		// closely, and the Highlight button is what they are about to press.
		const selection = window.getSelection();
		const live = !!selection && !selection.isCollapsed;
		if (live && (!selectedAtDown || event.detail > 1)) {
			if ((event.target as Element | null)?.closest?.('.flow a[href]')) event.preventDefault();
			return;
		}

		// A link in the book is a destination, not a page turn. Followed as a
		// normal navigation it would leave the reader; handled here it updates
		// the address instead, so back and forward work and the link can still
		// be copied.
		const link = (event.target as Element | null)?.closest?.('.flow a[href]');
		if (link) {
			event.preventDefault();
			const href = link.getAttribute('href') ?? '';
			if (href.startsWith('?')) {
				void goto(page.url.pathname + href, { noScroll: true, keepFocus: true });
			}
			return;
		}

		// A highlight is a target in its own right; turning the page instead
		// would make an existing note unreachable.
		const mark = markAt(event.target);
		if (mark) {
			activeNote = mark;
			noteDraft = mark.note ?? '';
			return;
		}

		// A selection that was already showing: this tap dismisses it, and
		// does nothing else.
		if (pendingSelection || selectedAtDown) {
			pendingSelection = null;
			selection?.removeAllRanges();
			return;
		}

		const x = event.clientX - viewport.getBoundingClientRect().left;
		const third = viewport.clientWidth / 3;
		if (x < third) prevPage();
		else if (x > third * 2) nextPage();
		else chromeVisible = !chromeVisible;
	}

	// Where the address says the reader should be.
	//
	// Set by a link inside the book, by a shared URL, and by the back button -
	// all three arrive the same way, which is why the address is what drives
	// this rather than the click handler doing the work itself.
	//
	// `handled` is a plain variable, not state: it exists to stop this effect
	// reacting to its own navigation, and making it reactive would do exactly
	// that.
	let handled = '';
	$effect(() => {
		const search = page.url.search;
		const spineParam = page.url.searchParams.get('spine');
		const frag = page.url.searchParams.get('frag');
		if (spineParam === null || search === handled) return;
		handled = search;

		const target = Number(spineParam);
		if (!Number.isInteger(target) || target < 0) return;

		pendingFragment = frag;
		if (target === spineIndex) {
			// Already in the right document: only the position changes, and
			// reloading it would throw away a rendered chapter to land in it.
			jumpToFragment();
		} else {
			void loadSpine(target);
		}
	});

	/** Moves to a fragment inside the chapter already on screen. */
	function jumpToFragment() {
		if (!pendingFragment || !flow) return;
		const wanted = pendingFragment;
		pendingFragment = null;

		const target = flow.querySelector('#' + CSS.escape(wanted));
		if (!target) return;
		pageIndex = pageOf(target);
		// Setting the index alone moved nothing: a link into the chapter already
		// on screen waited for whatever re-measure happened to come next.
		applyOffset(false);
		settledOn();
		here = elementPath(target, flow) || here;
	}

	$effect(() => {
		loadPrefs();
	});

	$effect(() => {
		const id = page.params.id;
		if (id) void loadManifest(id);
	});

	// Re-measure when typography or the viewport changes: both change how many
	// columns the same text occupies, and so which page a paragraph is on. The
	// paragraph is what is kept, not the page number - see relayout().
	$effect(() => {
		void prefs.fontSize;
		void prefs.lineHeight;
		void prefs.measure;
		void prefs.fontFamily;
		void prefs.justify;
		void html;

		if (!viewport) return;
		const observer = new ResizeObserver(() => relayout());
		observer.observe(viewport);
		// Same reasoning as settled(): measuring must not depend on a frame that
		// may never arrive.
		void settled().then(() => relayout());
		return () => observer.disconnect();
	});

	$effect(() => {
		savePrefs();
	});

	// A reader who closes the tab mid-chapter must not lose their place.
	//
	// Both listeners are named and both are removed. The visibilitychange one
	// used to be an anonymous closure, which cannot be removed: every book
	// opened in a session left one more permanently attached to `document`,
	// each holding that component's whole chapter, manifest and marks alive.
	// An evening's browsing grew the tab's memory with no way to release it.
	//
	// The cleanup also FLUSHES. pagehide fires on a real unload and not on
	// client-side routing, so tapping "← Library" within the debounce window
	// simply lost the last page turn - read to page 40, leave, come back to 39.
	$effect(() => {
		// Captured now, so the flush on the way out names the book that was
		// actually being read.
		const bookID = page.params.id;
		const flush = () => void saveNow(bookID);
		const onHidden = () => {
			if (document.visibilityState === 'hidden') flush();
		};
		window.addEventListener('pagehide', flush);
		document.addEventListener('visibilitychange', onHidden);
		return () => {
			window.removeEventListener('pagehide', flush);
			document.removeEventListener('visibilitychange', onHidden);
			flush();
		};
	});

	const progress = $derived(pageCount > 1 ? (pageIndex / (pageCount - 1)) * 100 : 0);

	/** Every contents entry, nested ones included, in the book's order. */
	const tocEntries = $derived(flattenToc(manifest?.toc));

	/**
	 * What the chrome says the reader is looking at.
	 *
	 * The chapter's own name when the contents list has one for it, because
	 * "Use Testing to Develop Better Software Faster" tells somebody where they
	 * are and "95 of 104" does not. The count stays as a fallback and as the
	 * second line: a book's chapter names repeat and its numbers do not.
	 *
	 * State rather than derived, because it depends on where the contents'
	 * targets fall on the page - layout, which nothing reactive can see. It is
	 * placed whenever the page moves; see placeChapter().
	 */
	let chapterTitle = $state('');
	/** The entry the reader is inside, to mark in the contents. */
	let currentToc = $state<TOCEntry | null>(null);

	/**
	 * Works out which chapter the page on screen belongs to.
	 *
	 * The contents entry whose target the reader has reached - not merely the
	 * last one naming this file, which put a reader on chapter one of a file
	 * holding ten under "CHAPTER X." for the whole file (see toc.ts). A book
	 * whose contents say nothing about here falls back to its headings.
	 *
	 * Called from applyOffset, the one place every page change passes through.
	 */
	function placeChapter() {
		if (!flow || !manifest) return;
		const root = flow;
		const entry = currentEntry(tocEntries, spineIndex, pageIndex, (fragment) => {
			const target = root.querySelector('#' + CSS.escape(fragment));
			return target ? pageOf(target) : null;
		});
		currentToc = entry;
		if (entry && entry.spineIndex === spineIndex) {
			chapterTitle = entry.title;
			return;
		}
		// Nothing in the contents starts in this file before here. Its own
		// headings say more than an entry from a file already finished.
		const heading = headingBefore(
			[...root.querySelectorAll('h1, h2, h3')].map((h) => ({
				title: (h.textContent ?? '').replace(/\s+/g, ' ').trim(),
				page: pageOf(h)
			})),
			pageIndex
		);
		chapterTitle = heading || entry?.title || '';
	}

	/**
	 * Opens a contents entry where it points: the element its fragment names,
	 * not the top of the file. Chapters that share a file are only reachable
	 * this way.
	 */
	function openEntry(entry: TOCEntry) {
		if (entry.spineIndex < 0) return;
		showToc = false;
		const fragment = entry.fragment || null;
		if (entry.spineIndex === spineIndex && !loading) {
			if (fragment) {
				pendingFragment = fragment;
				jumpToFragment();
			} else {
				pageIndex = 0;
				applyOffset(false);
				settledOn();
			}
			return;
		}
		pendingFragment = fragment;
		void loadSpine(entry.spineIndex);
	}

	const chapterCount = $derived(
		manifest ? `${spineIndex + 1} of ${manifest.spine.length}` : ''
	);
</script>

<!-- One contents entry and whatever it contains, at any depth. -->
{#snippet tocItem(entry: TOCEntry)}
	<li>
		<button
			disabled={entry.spineIndex < 0}
			aria-current={entry === currentToc ? 'location' : undefined}
			onclick={() => openEntry(entry)}
		>
			{entry.title}
		</button>
		{#if entry.children?.length}
			<ul>
				{#each entry.children as child, j (j)}
					{@render tocItem(child)}
				{/each}
			</ul>
		{/if}
	</li>
{/snippet}

<!-- The book's name, so a tab and a history entry say which book. Every one of
     them read "Reading · Lexarii" before, which is useless with four open. -->
<svelte:head><title>{bookTitle ? `${bookTitle} · Lexarii` : 'Reading · Lexarii'}</title></svelte:head>
<svelte:window onkeydown={onKey} />

<div
	class="reader"
	data-theme={prefs.theme}
	style="
		--reader-size: {prefs.fontSize}px;
		--reader-leading: {prefs.lineHeight};
		--reader-measure: {prefs.measure}rem;
		--reader-family: {prefs.fontFamily === 'serif' ? 'var(--font-serif)' : 'var(--font-sans)'};
		--reader-align: {prefs.justify ? 'justify' : 'start'};
	"
>
	{#if notice}
		<!--
			Said over the book rather than instead of it: whatever failed, the
			chapter on screen is still readable and still theirs.
		-->
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
		<!--
			The page surface. Edges turn, the centre toggles chrome, and every
			action also has a keyboard path.
		-->
		<div
			class="viewport"
			bind:this={viewport}
			onpointerdown={onPointerDown}
			onclick={onTap}
			onmouseup={onSelection}
			ontouchend={onSelection}
			role="presentation"
			aria-busy={loading}
		>
			<!-- eslint-disable-next-line svelte/no-at-html-tags -- sanitised server-side -->
			<div
				class="flow"
				bind:this={flow}
				onloadcapture={pictureArrived}
				onerrorcapture={pictureArrived}
			>{@html html}</div>
		</div>

		{#if furthestHint}
			<div class="furthest" role="status">
				<span>You had read to {furthestHint.percent}%</span>
				<button onclick={jumpToFurthest}>Jump there</button>
				<button class="dismiss" onclick={() => (furthestHint = null)} aria-label="Stay here">×</button>
			</div>
		{/if}

		<div class="chrome" class:hidden={!chromeVisible}>
			<header>
				<a class="control" href="/" aria-label="Back to the library">←</a>
				<div class="where">
					{#if bookTitle}
						<a class="book" href={workId ? `/works/${workId}` : '/'} title={bookTitle}>
							{bookTitle}
						</a>
					{/if}
					<span class="chapter">
						{chapterTitle || chapterCount}
						{#if chapterTitle}<span class="count">· {chapterCount}</span>{/if}
					</span>
				</div>
				<!-- Asking about the book in front of you. The scope has always
				     reached retrieval; nothing offered it, so the only way to ask
				     about this book was to ask the whole library and hope. -->
				<a
					class="control ask"
					href="/ask?book={page.params.id}"
					title="Ask about this book"
					aria-label="Ask about this book">Ask</a>
				<button class="control" onclick={() => (showToc = !showToc)} aria-label="Contents">☰</button>
				<button class="control" onclick={() => (showSettings = !showSettings)} aria-label="Reading settings">Aa</button>
			</header>

			<footer>
				<button class="control" onclick={prevPage} aria-label="Previous page">‹</button>
				<div class="progress" role="progressbar" aria-valuenow={Math.round(progress)} aria-valuemin={0} aria-valuemax={100}>
					<div class="bar" style="width: {progress}%"></div>
				</div>
				<span class="pages">{pageIndex + 1} / {pageCount}</span>
				<button class="control" onclick={nextPage} aria-label="Next page">›</button>
			</footer>
		</div>

		{#if showToc && manifest}
			<aside class="panel toc">
				<h2>Contents</h2>
				{#if manifest.toc.length === 0}
					<p class="empty">This book has no table of contents.</p>
				{/if}
				<ul>
					{#each manifest.toc as entry, i (i)}
						{@render tocItem(entry)}
					{/each}
				</ul>
			</aside>
		{/if}

		{#if pendingSelection}
			<div
				class="selection-actions"
				style="left: {pendingSelection.x}px; top: {pendingSelection.y}px"
			>
				<button onclick={() => highlightSelection(false)}>Highlight</button>
				<button onclick={() => highlightSelection(true)}>Note</button>
			</div>
		{/if}

		{#if activeNote}
			<aside class="panel note">
				<h2>{activeNote.type === 'note' ? 'Note' : 'Highlight'}</h2>

				{#if activeNote.shared}
					<!-- Somebody else's mark, visible because they shared it. Readable
					     and never editable: the store matches on user_id for every
					     write, so offering Save here would only produce a refusal. -->
					<p class="by">
						{activeNote.author || 'Someone else'} wrote this
					</p>
				{/if}

				{#if activeNote.orphaned}
					<!--
						The anchor stopped resolving - a re-ingest, a different edition.
						The mark is shown with the words it was made against rather than
						quietly disappearing.
					-->
					<p class="orphan">
						This no longer matches the book's text. It is kept here with what it
						was written about.
					</p>
				{/if}

				{#if activeNote.selectedText}
					<blockquote>{activeNote.selectedText}</blockquote>
				{/if}

				{#if activeNote.shared}
					{#if activeNote.note}
						<p class="their-note">{activeNote.note}</p>
					{:else}
						<p class="their-note empty">They highlighted this without a note.</p>
					{/if}

					<div class="note-actions">
						<button onclick={() => (activeNote = null)}>Close</button>
					</div>
				{:else}
					<textarea bind:value={noteDraft} placeholder="Add a note…" rows="5"></textarea>

					<div class="note-actions">
						<button class="primary" onclick={saveNote}>Save</button>
						<button onclick={() => (activeNote = null)}>Close</button>
						<button class="danger" onclick={() => activeNote && removeMark(activeNote)}>
							Delete
						</button>
					</div>
				{/if}
			</aside>
		{/if}

		{#if showSettings}
			<aside class="panel settings">
				<h2>Reading</h2>

				<div class="row">
					<span class="label">Theme</span>
					<div class="options">
						{#each ['light', 'sepia', 'dark', 'black'] as theme (theme)}
							<button
								class="swatch {theme}"
								class:active={prefs.theme === theme}
								onclick={() => (prefs.theme = theme as Prefs['theme'])}
								aria-label={theme}
							></button>
						{/each}
					</div>
				</div>

				<div class="row">
					<span class="label">Typeface</span>
					<div class="options">
						<button class:active={prefs.fontFamily === 'serif'} onclick={() => (prefs.fontFamily = 'serif')}>
							Serif
						</button>
						<button class:active={prefs.fontFamily === 'sans'} onclick={() => (prefs.fontFamily = 'sans')}>
							Sans
						</button>
					</div>
				</div>

				<label class="row">
					<span class="label">Size</span>
					<input type="range" min="14" max="30" step="1" bind:value={prefs.fontSize} />
					<span class="value">{prefs.fontSize}px</span>
				</label>

				<label class="row">
					<span class="label">Line height</span>
					<input type="range" min="1.2" max="2.2" step="0.05" bind:value={prefs.lineHeight} />
					<span class="value">{prefs.lineHeight.toFixed(2)}</span>
				</label>

				<!--
					Measure is the single most important setting: 60-75 characters is
					where reading is comfortable, and it is what the column width is
					actually for.
				-->
				<label class="row">
					<span class="label">Width</span>
					<input type="range" min="20" max="52" step="1" bind:value={prefs.measure} />
					<span class="value">{prefs.measure}rem</span>
				</label>

				<label class="row checkbox">
					<input type="checkbox" bind:checked={prefs.justify} />
					<span>Justify text</span>
				</label>

				<button class="reset" onclick={() => (prefs = { ...defaults })}>Reset to defaults</button>
			</aside>
		{/if}
	{/if}
</div>

<style>
	/*
	 * The reader owns the whole screen and its own palette: it is deliberately
	 * not the library's theme, because a reading surface is a different object
	 * from an application chrome.
	 */
	.reader {
		position: fixed;
		inset: 0;
		background: var(--page-bg);
		color: var(--page-fg);
		overflow: hidden;
		--page-bg: #faf8f4;
		--page-fg: #1f1c18;
		--page-muted: #6b6558;
		--page-chrome: rgb(255 255 255 / 0.92);
		/* How far the chrome's bars reach into the page: a 44px control and
		   the bar's padding, the safe area included. See .viewport. */
		--bar-top: calc(44px + var(--space-2) + max(var(--space-2), env(safe-area-inset-top)));
		--bar-bottom: calc(44px + var(--space-2) + max(var(--space-2), env(safe-area-inset-bottom)));
	}

	.reader[data-theme='sepia'] {
		--page-bg: #f4ecd8;
		--page-fg: #3a2f21;
		--page-muted: #6f5f47;
		--page-chrome: rgb(244 236 216 / 0.94);
	}

	.reader[data-theme='dark'] {
		--page-bg: #16151a;
		--page-fg: #ddd9d2;
		--page-muted: #9b958a;
		--page-chrome: rgb(22 21 26 / 0.94);
	}

	.reader[data-theme='black'] {
		--page-bg: #000;
		--page-fg: #c8c4bd;
		--page-muted: #6d675e;
		--page-chrome: rgb(0 0 0 / 0.94);
	}

	.viewport {
		position: absolute;
		inset: 0;
		/* Generous vertical margins: whitespace is the point, not waste. The
		   horizontal padding centres the measure, and the flow inside is exactly
		   one column wide.

		   Never less than the chrome's bars, which are drawn over the page and
		   are showing whenever a book opens. 6vh alone is 48px on a phone and
		   the header is 60, so the top line of every page sat under it, and
		   the bottom one under the footer - clipped by the bars rather than by
		   the column. The space is kept when the chrome fades: changing it
		   would reflow the chapter on every tap in the middle of the page. */
		padding: max(6vh, calc(var(--bar-top) + var(--space-2)))
			max(var(--space-6), calc((100vw - var(--reader-measure)) / 2))
			max(6vh, calc(var(--bar-bottom) + var(--space-2)));
		overflow: hidden;
		cursor: pointer;
	}

	/*
	 * CSS multi-column pagination.
	 *
	 * The column box is exactly the viewport's content width, so each column is
	 * one page and advancing is a translateX of that width. The browser does the
	 * line breaking; nothing here measures glyphs.
	 */
	.flow {
		height: 100%;
		/* column-width and column-gap are set from JavaScript, where the real
		   content width is known; see measure(). */
		column-fill: auto;
		/* Transform only, so a page turn never triggers layout. */
		transition: transform var(--duration-page-turn) var(--ease-out);
		will-change: transform;

		font-family: var(--reader-family);
		font-size: var(--reader-size);
		line-height: var(--reader-leading);
		text-align: var(--reader-align);
		hyphens: auto;
	}

	/* Book content. The publisher's stylesheet was dropped server-side, so this
	   is the whole visual treatment. */
	/* The paragraph a citation landed on.
	   It fades rather than staying: a permanent mark on somebody else's
	   sentence would be indistinguishable from one of their own highlights,
	   and this is a pointer, not an annotation. */
	.flow :global(.cited) {
		animation: cited-fade 2.6s var(--ease-out) forwards;
		border-radius: var(--radius-sm);
	}

	@keyframes cited-fade {
		0%,
		55% {
			background: color-mix(in srgb, var(--accent) 22%, transparent);
		}
		100% {
			background: transparent;
		}
	}

	/* Somebody who has asked for less motion still needs to be told which
	   paragraph it is: the mark holds still instead of fading. */
	@media (prefers-reduced-motion: reduce) {
		.flow :global(.cited) {
			animation: none;
			background: color-mix(in srgb, var(--accent) 18%, transparent);
		}
	}

	.flow :global(h1),
	.flow :global(h2),
	.flow :global(h3) {
		font-weight: 600;
		line-height: 1.25;
		margin: 1.4em 0 0.6em;
		text-align: start;
		/* A heading stranded alone at the foot of a column reads as an orphan. */
		break-after: avoid;
	}

	.flow :global(p) {
		margin: 0 0 0.9em;
		orphans: 2;
		widows: 2;
	}

	.flow :global(img) {
		max-width: 100%;
		height: auto;
		/* An image taller than a column would otherwise be sliced in half. */
		max-height: 80vh;
		object-fit: contain;
		break-inside: avoid;
	}

	.flow :global(blockquote) {
		margin: 1em 0;
		padding-left: 1em;
		border-left: 2px solid var(--page-muted);
		color: var(--page-muted);
	}

	.flow :global(pre) {
		font-family: var(--font-mono);
		font-size: 0.85em;
		line-height: 1.45;
		overflow-x: auto;
		white-space: pre-wrap;
		break-inside: avoid;
	}

	.flow :global(code) {
		font-family: var(--font-mono);
		font-size: 0.88em;
	}

	.flow :global(table) {
		border-collapse: collapse;
		font-size: 0.9em;
		break-inside: avoid;
	}

	.flow :global(td),
	.flow :global(th) {
		border: 1px solid var(--page-muted);
		padding: 0.3em 0.5em;
	}

	/* Only a link that goes somewhere looks like one.
	   Books use empty anchors as link TARGETS - <a id="fn12"/> marking where a
	   footnote points - and styling those underlines nothing, or in a book that
	   self-closes them, everything. A reference this reader cannot follow keeps
	   its text and loses its href, and must not look clickable either. */
	.flow :global(a[href]) {
		color: inherit;
		text-decoration: underline;
		text-underline-offset: 2px;
		cursor: pointer;
	}

	.flow :global(a[href]:hover) {
		text-decoration-thickness: 2px;
	}

	/* Chrome fades out of the way. The text is the interface. */
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
		font-size: var(--text-sm);
		color: var(--page-muted);
	}

	.chrome header {
		top: 0;
		padding-top: max(var(--space-2), env(safe-area-inset-top));
	}

	.chrome footer {
		bottom: 0;
		padding-bottom: max(var(--space-2), env(safe-area-inset-bottom));
	}

	/* Where the reader is: the book on top, the chapter under it. Two lines
	   because a book title and a chapter title are both long, and putting them
	   on one line means truncating whichever comes second. */
	.where {
		flex: 1;
		min-width: 0;
		display: flex;
		flex-direction: column;
		justify-content: center;
		gap: 1px;
		/* The chrome is a fixed height; this must not stretch it past the
		   44px the page's padding allows for. */
		max-height: 44px;
		overflow: hidden;
	}

	.book {
		font-size: var(--text-sm);
		color: var(--fg);
		text-decoration: none;
		white-space: nowrap;
		overflow: hidden;
		text-overflow: ellipsis;
	}

	.book:hover {
		text-decoration: underline;
	}

	.chapter {
		font-size: var(--text-xs);
		color: var(--fg-muted);
		white-space: nowrap;
		overflow: hidden;
		text-overflow: ellipsis;
	}

	.count {
		color: var(--fg-subtle);
		font-variant-numeric: tabular-nums;
	}

	.control {
		display: inline-flex;
		align-items: center;
		justify-content: center;
		min-width: 44px;
		min-height: 44px;
		border: 0;
		background: none;
		color: var(--page-muted);
		font: inherit;
		font-size: var(--text-base);
		cursor: pointer;
		text-decoration: none;
		border-radius: var(--radius-sm);
	}

	.control.ask {
		font-size: var(--text-sm);
	}

	.control:hover {
		color: var(--page-fg);
	}

	.progress {
		flex: 1;
		height: 2px;
		background: color-mix(in srgb, var(--page-muted) 30%, transparent);
		border-radius: 1px;
	}

	.bar {
		height: 100%;
		background: var(--page-muted);
		border-radius: 1px;
	}

	.pages {
		font-variant-numeric: tabular-nums;
		min-width: 4.5rem;
		text-align: right;
	}

	/* Sits above the footer rather than over the text: an offer, not an
	   interruption, and it disappears the moment it is answered either way. */
	.furthest {
		position: absolute;
		left: 50%;
		bottom: calc(env(safe-area-inset-bottom) + 4.5rem);
		transform: translateX(-50%);
		display: flex;
		align-items: center;
		gap: var(--space-3);
		max-width: calc(100vw - 2 * var(--space-4));
		padding: var(--space-2) var(--space-3);
		background: var(--page-chrome);
		backdrop-filter: blur(12px);
		border: 1px solid color-mix(in srgb, var(--page-muted) 25%, transparent);
		border-radius: 999px;
		font-size: var(--text-sm);
		color: var(--page-muted);
		z-index: 3;
	}

	.furthest span {
		white-space: nowrap;
		overflow: hidden;
		text-overflow: ellipsis;
	}

	.furthest button {
		min-height: 44px;
		padding: 0 var(--space-3);
		border: 0;
		border-radius: 999px;
		background: transparent;
		color: var(--page-fg);
		font: inherit;
		font-weight: 600;
		cursor: pointer;
	}

	.furthest button:hover {
		background: color-mix(in srgb, var(--page-muted) 15%, transparent);
	}

	.furthest .dismiss {
		min-width: 44px;
		padding: 0;
		font-weight: 400;
		color: var(--page-muted);
	}

	.panel {
		position: absolute;
		top: 0;
		right: 0;
		bottom: 0;
		width: min(22rem, 90vw);
		padding: var(--space-6) var(--space-4);
		padding-top: max(var(--space-6), env(safe-area-inset-top));
		background: var(--page-chrome);
		backdrop-filter: blur(12px);
		border-left: 1px solid color-mix(in srgb, var(--page-muted) 25%, transparent);
		overflow-y: auto;
		z-index: 2;
	}

	.panel h2 {
		font-size: var(--text-sm);
		text-transform: uppercase;
		letter-spacing: 0.06em;
		color: var(--page-muted);
		margin: 0 0 var(--space-4);
	}

	.toc ul {
		list-style: none;
		margin: 0;
		padding: 0;
	}

	.toc ul ul {
		padding-left: var(--space-4);
	}

	.toc button {
		display: block;
		width: 100%;
		text-align: start;
		min-height: 44px;
		padding: var(--space-2) 0;
		border: 0;
		background: none;
		color: var(--page-fg);
		font: inherit;
		font-size: var(--text-sm);
		cursor: pointer;
	}

	.toc button[disabled] {
		color: var(--page-muted);
		cursor: default;
	}

	/* Where the reader is, so the list opens on something familiar. */
	.toc button[aria-current] {
		font-weight: 600;
	}

	.row {
		display: flex;
		align-items: center;
		gap: var(--space-3);
		min-height: 44px;
		margin-bottom: var(--space-3);
		font-size: var(--text-sm);
	}

	.label {
		min-width: 5.5rem;
		color: var(--page-muted);
	}

	.value {
		min-width: 3.5rem;
		text-align: right;
		color: var(--page-muted);
		font-variant-numeric: tabular-nums;
	}

	.options {
		display: flex;
		gap: var(--space-2);
	}

	.options button {
		min-height: 44px;
		padding: 0 var(--space-3);
		border: 1px solid color-mix(in srgb, var(--page-muted) 30%, transparent);
		border-radius: var(--radius-sm);
		background: none;
		color: var(--page-fg);
		font: inherit;
		font-size: var(--text-sm);
		cursor: pointer;
	}

	.options button.active {
		border-color: var(--page-fg);
	}

	.swatch {
		width: 44px;
		height: 44px;
		border-radius: var(--radius-sm);
		border: 1px solid color-mix(in srgb, var(--page-muted) 40%, transparent);
		cursor: pointer;
	}

	.swatch.light {
		background: #faf8f4;
	}
	.swatch.sepia {
		background: #f4ecd8;
	}
	.swatch.dark {
		background: #16151a;
	}
	.swatch.black {
		background: #000;
	}
	.swatch.active {
		outline: 2px solid var(--page-fg);
		outline-offset: 2px;
	}

	.checkbox {
		gap: var(--space-2);
	}

	input[type='range'] {
		flex: 1;
	}

	.reset {
		margin-top: var(--space-4);
		min-height: 44px;
		width: 100%;
		border: 1px solid color-mix(in srgb, var(--page-muted) 30%, transparent);
		border-radius: var(--radius-sm);
		background: none;
		color: var(--page-muted);
		font: inherit;
		font-size: var(--text-sm);
		cursor: pointer;
	}

	.message {
		display: flex;
		flex-direction: column;
		gap: var(--space-4);
		align-items: center;
		justify-content: center;
		height: 100%;
		padding: var(--space-6);
		text-align: center;
	}

	.error {
		color: var(--danger);
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
		box-shadow: var(--shadow-md, 0 2px 8px rgb(0 0 0 / 0.2));
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

	.empty {
		color: var(--page-muted);
		font-size: var(--text-sm);
	}

	/* Highlights. Painted over the book's own text, so they must be legible in
	   every theme without changing the line height - a highlight that reflows
	   the page would move the words it marks. */
	.flow :global(mark.sm-highlight) {
		background: color-mix(in srgb, var(--page-fg) 18%, transparent);
		color: inherit;
		border-radius: 2px;
		padding: 0 1px;
		cursor: pointer;
	}

	.flow :global(mark.sm-highlight[data-colour='yellow']) {
		background: color-mix(in srgb, #e9c46a 45%, transparent);
	}

	.selection-actions {
		position: fixed;
		transform: translate(-50%, -130%);
		display: flex;
		gap: var(--space-1);
		padding: var(--space-1);
		border-radius: var(--radius-md);
		background: var(--page-chrome);
		backdrop-filter: blur(8px);
		box-shadow: var(--shadow-raised);
		z-index: 3;
	}

	.selection-actions button {
		min-height: 44px;
		padding: 0 var(--space-3);
		border: 0;
		border-radius: var(--radius-sm);
		background: none;
		color: var(--page-fg);
		font: inherit;
		font-size: var(--text-sm);
		cursor: pointer;
	}

	.selection-actions button:hover {
		background: color-mix(in srgb, var(--page-fg) 10%, transparent);
	}

	.panel.note blockquote {
		margin: 0 0 var(--space-4);
		padding-left: var(--space-3);
		border-left: 2px solid color-mix(in srgb, var(--page-muted) 40%, transparent);
		font-family: var(--font-serif);
		font-size: var(--text-sm);
		color: var(--page-muted);
	}

	.panel.note textarea {
		width: 100%;
		padding: var(--space-3);
		border: 1px solid color-mix(in srgb, var(--page-muted) 30%, transparent);
		border-radius: var(--radius-sm);
		background: none;
		color: var(--page-fg);
		font: inherit;
		font-size: var(--text-sm);
		resize: vertical;
	}

	.orphan {
		font-size: var(--text-sm);
		color: var(--page-muted);
		border-left: 2px solid var(--danger);
		padding-left: var(--space-3);
		margin: 0 0 var(--space-4);
	}

	.by {
		margin: 0 0 var(--space-2);
		color: var(--page-muted);
		font-size: var(--text-sm);
	}

	.their-note {
		margin: 0 0 var(--space-3);
		white-space: pre-wrap;
	}

	.their-note.empty {
		color: var(--page-muted);
		font-style: italic;
	}

	.note-actions {
		display: flex;
		gap: var(--space-2);
		margin-top: var(--space-3);
	}

	.note-actions button {
		min-height: 44px;
		padding: 0 var(--space-3);
		border: 1px solid color-mix(in srgb, var(--page-muted) 30%, transparent);
		border-radius: var(--radius-sm);
		background: none;
		color: var(--page-fg);
		font: inherit;
		font-size: var(--text-sm);
		cursor: pointer;
	}

	.note-actions .primary {
		background: var(--page-fg);
		color: var(--page-bg);
		border-color: var(--page-fg);
	}

	.note-actions .danger {
		color: var(--danger);
		margin-left: auto;
	}

	/* A page turn is motion. Respect a user who has asked for less of it. */
	@media (prefers-reduced-motion: reduce) {
		.flow {
			transition: none;
		}
	}
</style>

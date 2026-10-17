// SPDX-License-Identifier: AGPL-3.0-or-later

/**
 * Rendering book covers, in the browser, because the server cannot.
 *
 * A PDF's cover is its first page. Rasterising that needs a renderer, and the
 * server has none: the good ones need cgo, which `CGO_ENABLED=0` rules out and
 * which is the same reason the model link is HTTP rather than bindings. The
 * browser already has a rasteriser, already uses it to show these books, and is
 * idle while somebody browses a shelf. So the work happens here and the result
 * is uploaded once.
 *
 * Deliberately unhurried. This runs one book at a time, only while the library
 * page is open, and gives up quietly on anything that fails. A cover is a
 * nicety: it must never compete with the page someone is actually reading, and
 * a book that will not render should cost one attempt, not one per visit.
 */

import * as api from '$api/client';

/**
 * How wide a stored cover is, in pixels.
 *
 * Cards are about 180px wide and can be on a 2x display, so 400 covers them
 * with room to spare. Storing the full page would mean megabytes per book to
 * show something the size of a postage stamp.
 */
const coverWidth = 400;

/**
 * WebP at this quality is a few tens of kilobytes for a page of text, and
 * indistinguishable from the original at card size. Anything higher is spent on
 * detail nobody sees.
 */
const coverQuality = 0.8;

/** Books whose cover failed in this session. One attempt each, not one per render. */
const failed = new Set<string>();

let running = false;

/** Whether a run is going, so the shelf can offer to start one or stop it. */
export function renderingCovers(): boolean {
	return running;
}

/**
 * Renders and uploads covers for books that have none.
 *
 * Reports progress as it goes, because this only runs while the library page is
 * open: close the tab at book 97 of 167 and the rest wait for the next visit.
 * That is fine, but it has to be VISIBLE, or the result is a shelf where some
 * books mysteriously have covers and others do not.
 *
 * The remaining count is the server's, not this loop's arithmetic. The queue is
 * handed out 24 at a time, so counting the batch reported "24 left" after every
 * round of 24 - which reads as no progress at all.
 *
 * Safe to call repeatedly: a run already in progress is left to finish rather
 * than joined by a second one.
 */
export async function renderMissingCovers(
	onProgress?: (done: number, remaining: number) => void,
	signal?: AbortSignal
): Promise<void> {
	if (running) return;
	running = true;

	try {
		let pending = await api.pendingCovers(signal);
		onProgress?.(0, pending.remaining);
		if (pending.items.length === 0) return;

		// eslint-disable-next-line @typescript-eslint/no-explicit-any
		const pdfjs: any = await import('pdfjs-dist');
		const worker = await import('pdfjs-dist/build/pdf.worker.min.mjs?url');
		pdfjs.GlobalWorkerOptions.workerSrc = worker.default;

		// Batch after batch until the shelf is covered. The server hands out a
		// page at a time; stopping after one would mean a 168-book library
		// needed seven visits to fill in, which is the sort of thing nobody
		// notices is broken because it does eventually finish.
		let done = 0;
		let remaining = pending.remaining;
		while (pending.items.length > 0) {
			let rendered = 0;
			onProgress?.(done, remaining);

			for (const target of pending.items) {
				if (signal?.aborted) return;
				if (failed.has(target.bookFileId)) continue;

				try {
					const blob = await renderFirstPage(pdfjs, target.bookFileId, signal);
					if (!blob) {
						failed.add(target.bookFileId);
						continue;
					}
					await api.putCover(target.bookFileId, blob);
					rendered++;
					done++;
					remaining = Math.max(0, remaining - 1);
					onProgress?.(done, remaining);
				} catch {
					// A book that will not render is not an error anybody needs
					// to see. It keeps its typographic cover, which is a
					// perfectly good cover.
					failed.add(target.bookFileId);
				}
			}

			// Nothing worked this round, so the next batch is the same batch:
			// stop rather than ask the server the same question forever.
			if (rendered === 0) return;
			if (signal?.aborted) return;
			pending = await api.pendingCovers(signal);
			// The server's count again, so a cover rendered in another tab or
			// on another device is reflected rather than double-counted.
			remaining = pending.remaining;
		}
		onProgress?.(done, 0);
	} catch {
		/* no covers this time; the shelf is unaffected */
	} finally {
		running = false;
	}
}

/** Renders page one of a PDF to a downscaled image. */
async function renderFirstPage(
	// eslint-disable-next-line @typescript-eslint/no-explicit-any
	pdfjs: any,
	bookFileId: string,
	signal?: AbortSignal
): Promise<Blob | null> {
	// The per-page endpoint rather than the whole file: it sends one page
	// instead of fifty megabytes, and it works for a book shared to be read,
	// which the file endpoint deliberately refuses.
	const response = await fetch(
		`/api/v1/books/${encodeURIComponent(bookFileId)}/page/1?prefetch=1`,
		{ credentials: 'same-origin', signal }
	);
	if (!response.ok) return null;

	const bytes = new Uint8Array(await response.arrayBuffer());
	const doc = await pdfjs.getDocument({ data: bytes }).promise;

	try {
		const page = await doc.getPage(1);
		const unscaled = page.getViewport({ scale: 1 });
		const viewport = page.getViewport({ scale: coverWidth / unscaled.width });

		const canvas = document.createElement('canvas');
		canvas.width = Math.round(viewport.width);
		canvas.height = Math.round(viewport.height);

		const context = canvas.getContext('2d');
		if (!context) return null;

		// A PDF page is transparent where nothing is drawn, and a transparent
		// cover on a dark theme renders as white text on white. Paint the page
		// white first, the way paper is.
		context.fillStyle = '#ffffff';
		context.fillRect(0, 0, canvas.width, canvas.height);

		await page.render({ canvasContext: context, viewport }).promise;

		return await new Promise<Blob | null>((resolve) =>
			canvas.toBlob(resolve, 'image/webp', coverQuality)
		);
	} finally {
		void doc.destroy();
	}
}

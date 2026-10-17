// SPDX-License-Identifier: AGPL-3.0-or-later

import { afterEach, describe, expect, it, vi } from 'vitest';
import { ApiError, askStreaming } from './client';

function streamOf(...events: string[]): Response {
	return new Response(events.join(''), {
		status: 200,
		headers: { 'Content-Type': 'text/event-stream' }
	});
}

describe('askStreaming', () => {
	afterEach(() => vi.unstubAllGlobals());

	it('ends with the code and message the server sent in its error event', async () => {
		vi.stubGlobal(
			'fetch',
			vi.fn(async () =>
				streamOf(
					'event: conversation\ndata: {"id":"c1","title":"q","updatedAt":"2026-01-01T00:00:00Z"}\n\n',
					'event: error\ndata: {"code":"llm_unavailable","message":"The assistant is busy right now. Try again in a minute.","details":{"retryAfterSeconds":60}}\n\n'
				)
			)
		);

		const failed = await askStreaming('why?', {}).catch((e: unknown) => e);
		expect(failed).toBeInstanceOf(ApiError);
		expect(failed).toMatchObject({
			code: 'llm_unavailable',
			status: 503,
			message: 'The assistant is busy right now. Try again in a minute.'
		});
	});

	it('reports a refused request before the stream opens in the usual shape', async () => {
		vi.stubGlobal(
			'fetch',
			vi.fn(
				async () =>
					new Response('{"error":{"code":"model_error","message":"refused"}}', {
						status: 502,
						headers: { 'Content-Type': 'application/json' }
					})
			)
		);

		const failed = await askStreaming('why?', {}).catch((e: unknown) => e);
		expect(failed).toMatchObject({ code: 'model_error', status: 502, message: 'refused' });
	});
});

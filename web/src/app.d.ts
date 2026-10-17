// SPDX-License-Identifier: AGPL-3.0-or-later
declare global {
	namespace App {
		interface Error {
			code?: string;
			message: string;
		}
	}
}

export {};

import { MediaQuery } from 'svelte/reactivity';

export class IsTouchDevice extends MediaQuery {
	constructor() {
		super('(hover: none)', false);
	}
}

export const isTouchDevice = new IsTouchDevice();

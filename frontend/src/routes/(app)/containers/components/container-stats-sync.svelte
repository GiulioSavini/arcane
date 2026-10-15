<script lang="ts">
	import { untrack } from 'svelte';

	import type { ContainerStatsManager } from './container-stats-manager.svelte';

	let {
		statsManager,
		targetIds
	}: {
		statsManager: ContainerStatsManager;
		targetIds: Set<string>;
	} = $props();

	// Only targetIds is tracked; the manager's own state reads and writes stay out of this effect.
	$effect(() => {
		const ids = targetIds;
		untrack(() => {
			statsManager.targetIds = ids;
		});
	});
</script>

import { Link, useParams } from 'react-router-dom';
import { LoadError, SkeletonPanel } from '@/components/primitives';
import { IncidentCard, type ActionBarItem } from '@/components/product';
import { CheckIcon, CopyIcon } from '@/icons';
import type { Incident } from '@/lib/types';
import { invalidateApiData, useApiData } from '@/lib/useApiData';
import { incident as incidentApi } from '@/lib/client';
import { useCopyToClipboard } from '@/lib/useCopyToClipboard';
import styles from './IncidentDetail.module.css';

/** The forwardable block: facts and evidence, never a paraphrase of them. */
function incidentContext(inc: Incident): string {
	return [
		`# ${inc.title}`,
		`Since ${inc.since}, ${inc.durationMinutes} minutes, ${inc.ongoing ? 'still open' : 'closed'}.`,
		'',
		'## Timeline',
		...inc.timeline.map((entry) => `- ${entry.time} ${entry.text}`),
		'',
		`## Log slice (${inc.logSlice.length} lines, trimmed)`,
		...inc.logSlice,
	].join('\n');
}

/** One incident, whole: timeline and log slice. No ack/resolve;
 *  the engine opens and closes incidents; a button without one is a lie. */
export function IncidentDetail() {
	const { id } = useParams<{ id: string }>();
	const { loading, failed, data } = useApiData(`incident:${id}`, () => incidentApi(id!));
	const inc = data as Incident | undefined;

	const [copiedContext, copyContext] = useCopyToClipboard();

	if (loading) {
		return <SkeletonPanel rows={4} label="Loading the incident" />;
	}
	if (failed || !inc) {
		return <LoadError what="this incident" onRetry={() => invalidateApiData(`incident:${id}`)} />;
	}

	const actions: ActionBarItem[] = [
		{
			id: 'share',
			label: copiedContext ? 'Copied!' : 'Share',
			variant: 'secondary',
			icon: copiedContext ? <CheckIcon width={12} height={12} /> : <CopyIcon width={12} height={12} />,
			tooltip:
				'Copies a ready-to-paste block: what broke, the timeline, and the error log lines — scrubbed, not the raw stream.',
			onFire: () => copyContext(incidentContext(inc)),
		},
	];

	return (
		<section className={styles.page}>
			<Link to="/incidents" className={styles.back}>
				← Incidents
			</Link>
			<IncidentCard
				incident={inc}
				actions={actions}
				result={
					inc.ongoing
						? { tone: 'open', text: 'Watching. The result shows up here.' }
						: { tone: inc.result?.status === 'still-down' ? 'still' : 'fixed', text: inc.result?.text ?? '' }
				}
			/>
		</section>
	);
}

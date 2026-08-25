import { useState } from 'react';
import { Link, useParams } from 'react-router-dom';
import { LoadError, SkeletonPanel } from '@/components/primitives';
import { IncidentCard, type ActionBarItem, type IncidentTriage } from '@/components/product';
import { CheckIcon, CopyIcon, PulseIcon } from '@/icons';
import type { Incident } from '@/lib/types';
import { invalidateApiData, useApiData } from '@/lib/useApiData';
import { incident as incidentApi, explainIncident } from '@/lib/client';
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

/** One incident, whole: timeline, log slice, Explain. No ack/resolve;
 *  the engine opens and closes incidents; a button without one is a lie. */
export function IncidentDetail() {
	const { id } = useParams<{ id: string }>();
	const { loading, failed, data } = useApiData(`incident:${id}`, () => incidentApi(id!));
	const inc = data as Incident | undefined;

	const [triage, setTriage] = useState<IncidentTriage | null>(null);
	const [copiedContext, copyContext] = useCopyToClipboard();

	if (loading) {
		return <SkeletonPanel rows={4} label="Loading the incident" />;
	}
	if (failed || !inc) {
		return <LoadError what="this incident" onRetry={() => invalidateApiData(`incident:${id}`)} />;
	}

	const actions: ActionBarItem[] = [
		{
			id: 'explain',
			label: triage ? 'Explain again' : 'Explain',
			variant: 'primary',
			icon: <PulseIcon width={12} height={12} />,
			tooltip:
				'AI reads this incident — its timeline and the log lines frozen when it fired — and answers what broke, why, how critical it is and what to run next.',
			onFire: () => {
				const pending: IncidentTriage = { loading: true };
				setTriage(pending);
				// The evidence is the server's: it assembles the incident's own
				// facts, timeline and frozen slice, so the front sends only the id.
				void explainIncident(inc.id)
					.then((res) => {
						setTriage((cur) => (cur === pending ? { loading: false, answer: res } : cur));
					})
					.catch((err: unknown) => {
						// Only the server's own words render as the note; the
						// machine-generated shapes leave the panel empty, not invented.
						const message = err instanceof Error ? err.message : '';
						const fromServer = message !== '' && !message.startsWith('HTTP ') && message !== 'unauthorized';
						setTriage((cur) =>
							cur === pending ? (fromServer ? { loading: false, note: message } : null) : cur,
						);
					});
			},
		},
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
				triage={triage}
				result={
					inc.ongoing
						? { tone: 'open', text: 'Watching. The result shows up here.' }
						: { tone: inc.result?.status === 'still-down' ? 'still' : 'fixed', text: inc.result?.text ?? '' }
				}
			/>
		</section>
	);
}

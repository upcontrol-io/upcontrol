import { Link } from 'react-router-dom';
import { PageHeader } from '@/components/layout';
import { EmptyState, LinkButton, LoadError, SkeletonPanel } from '@/components/primitives';
import { ErrorIcon } from '@/icons';
import type { Incident } from '@/lib/types';
import { formatDurationMinutes } from '@/lib/formatTime';
import { invalidateApiData, useApiData } from '@/lib/useApiData';
import { incidents as incidentsApi } from '@/lib/client';
import styles from './Incidents.module.css';

/**
 * The incident history: the latest 20, newest first — NO pagination, because
 * the contract has no parameters (public-first-split). A row opens the full
 * card with its timeline, log slice and Explain.
 */
export function Incidents() {
	const { loading, failed, data } = useApiData('incidents', () => incidentsApi());
	const items = (data as { items: Incident[] } | undefined)?.items ?? [];

	if (loading) {
		return <SkeletonPanel rows={3} label="Loading incidents" />;
	}
	if (failed) {
		return <LoadError what="your incidents" onRetry={() => invalidateApiData('incidents')} />;
	}

	return (
		<section className={styles.page}>
			<PageHeader
				title="Incidents"
				description="Every time a check failed, with the timeline and the log slice around it."
			/>
			{items.length === 0 ? (
				// Live and empty is its own fact, and on this screen it is the good
				// one — so it gets the same room a populated list would have, not a
				// grey sentence in the corner of an empty page. The title keeps the
				// exact wording incidents.spec.ts pins. The body says only what is
				// true: no incident has been recorded. It does NOT say nothing has
				// gone down, which would be a claim about checks this page never read.
				<EmptyState
					icon={<ErrorIcon width={22} height={22} />}
					title="No incidents recorded."
					body="An incident opens when a check fails, and closes when it recovers. None has been recorded on this instance, so there is nothing to show here yet."
					action={
						<LinkButton to="/monitors" variant="secondary" size="sm">
							See what is being watched
						</LinkButton>
					}
				/>
			) : (
				<>
					<div className={styles.list}>
						{items.map((inc) => (
							<Link key={inc.id} to={`/incidents/${inc.id}`} className={styles.row}>
								<span
									className={[styles.dot, inc.ongoing ? styles.dotDown : styles.dotClosed]
										.filter(Boolean)
										.join(' ')}
								/>
								<span className={styles.rowTitle}>{inc.title}</span>
								<span className={styles.rowMeta}>
									since {inc.since} · {formatDurationMinutes(inc.durationMinutes)} ·{' '}
									{inc.ongoing ? 'ongoing' : 'closed'}
								</span>
							</Link>
						))}
					</div>
					<p className={styles.foot}>The latest 20 incidents.</p>
				</>
			)}
		</section>
	);
}

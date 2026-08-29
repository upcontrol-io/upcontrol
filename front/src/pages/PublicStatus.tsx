import { useParams } from 'react-router-dom';
import { BrandMark, Callout, EmptyState, Skeleton, ThemeToggle, Tooltip } from '@/components/primitives';
import { useApiData, useDegradation } from '@/lib/useApiData';
import { publicStatus as publicStatusApi, isOffline, type components } from '@/lib/client';
import type { HealthStatus } from '@/lib/types';
import { useRevealOnScroll } from '@/lib/useRevealOnScroll';
import { formatMinutesAgo } from '@/lib/formatTime';
import { BASE_SPAN_SEC, bucketLabel, spanLabel } from '@/lib/statusBars';
import styles from './PublicStatus.module.css';

type BarStatus = 'ok' | 'check' | 'down' | 'nodata';

// The overall banner, from an ongoing incident and/or a component whose
// current bucket is not ok; it may never say "operational" during downtime.
type BannerState = 'ok' | 'check' | 'down';

const BANNER_COPY: Record<BannerState, string> = {
	ok: 'All systems operational',
	down: 'Some systems are down',
	check: 'Some systems are degraded',
};

/** The state a component is in right now: its newest bucket. `nodata` is not
 *  a state — it is the absence of one — and never counts as incident evidence. */
function currentState(bars: BarStatus[]): BarStatus | undefined {
	return bars[bars.length - 1];
}

function isFailure(status: BarStatus | undefined): status is 'down' | 'check' {
	return status === 'down' || status === 'check';
}

function watchDot(status: string): { background: string; border: string } {
	return status === 'nodata'
		? { background: 'transparent', border: '1px solid var(--line-strong)' }
		: { background: `var(--${status})`, border: 'none' };
}

/** One bar per bucket, oldest first — the API decides how many there are and
 *  what one covers (`barSpanSec`). */
function buildBars(statuses: HealthStatus[], spanSec: number): { status: BarStatus; label: string }[] {
	const count = statuses.length;
	return statuses.map((status, i) => ({
		status: status as BarStatus,
		label: bucketLabel(i, count, spanSec, status),
	}));
}

/** What a project's visitors see. No sample-data fallback (an unreachable
 *  backend is said out loud), and the footer switch is honest and removable. */
export function PublicStatus() {
	const { project: projectSlug } = useParams();
	const {
		data: page,
		loading,
		live,
		failed,
		error,
	} = useApiData<components['schemas']['PublicStatusResponse']>(`publicStatus:${projectSlug}`, () =>
		publicStatusApi(projectSlug ?? ''),
	);
	useRevealOnScroll();
	const degraded = useDegradation(`publicStatus:${projectSlug}`);

	// A refusal that is not "nobody answered": the page does not exist.
	if (failed && !isOffline(error)) {
		return (
			<div className={styles.page}>
				<StatusHeader host={projectSlug ?? 'status'} />
				<main className={styles.main}>
					<EmptyState
						framed={false}
						title="There is no status page at this address."
						body="The link may be mistyped, or the page may have been removed."
					/>
				</main>
			</div>
		);
	}

	// Nothing has answered yet — skeletons, never an invented page.
	if (loading || !live || !page) {
		return (
			<div className={styles.page}>
				<StatusHeader host={projectSlug ?? 'status'} />
				<main className={styles.main}>
					{/* Settled-offline is its own fact: a refusal and an outage must
					    look different here, and there is no sample page to stand in. */}
					{failed && (
						<Callout tone="note" title="Backend not reachable">
							This status page is temporarily unreachable. What should be here cannot be shown right now.
						</Callout>
					)}
					<section className={styles.banner}>
						<Skeleton width={260} height={30} />
					</section>
					<section className={styles.componentList}>
						<div className={styles.componentRow}>
							<Skeleton width={200} height={14} />
							<Skeleton height={26} />
						</div>
					</section>
				</main>
			</div>
		);
	}

	const shownComponents = (page.components ?? []).filter((component) => component?.shown);
	const incidents = page.incidents ?? [];

	const banner: BannerState =
		shownComponents.some((c) => currentState((c.bars ?? []) as BarStatus[]) === 'down') ||
		incidents.some((i) => i.ongoing && i.status === 'down')
			? 'down'
			: shownComponents.some((c) => isFailure(currentState((c.bars ?? []) as BarStatus[]))) ||
				  incidents.some((i) => i.ongoing)
				? 'check'
				: 'ok';

	const barSpan = shownComponents[0]?.barSpanSec ?? BASE_SPAN_SEC;
	const network = page.network ?? [];
	const host = page.title || projectSlug || 'status';
	const updatedMinutes = page.updatedAt ? Math.floor((Date.now() - Date.parse(page.updatedAt)) / 60_000) : 0;
	const updatedAgo = Number.isFinite(updatedMinutes) ? formatMinutesAgo(updatedMinutes) : 'just now';
	// Default true: an older backend that does not send the field draws the
	// line, and switching it off is the owner's explicit act.
	const poweredBy = page.poweredBy !== false;

	return (
		<div className={styles.page}>
			<StatusHeader host={host} />

			<main className={styles.main}>
				{/* Fires when a live answer is on screen and a later refetch failed;
				    the next successful poll clears it on its own. */}
				{degraded && (
					<Callout tone="note" title="Connection lost">
						What you see below is the last known state, not a live page.
					</Callout>
				)}

				<section className={`${styles.banner} ${styles[`banner_${banner}`]}`} data-reveal="" data-reveal-delay={60}>
					<span className={`${styles.bannerDot} ${styles[`bannerDot_${banner}`]}`} />
					<div className={styles.bannerText}>
						<h1 className={styles.bannerTitle}>{BANNER_COPY[banner]}</h1>
						<span className={styles.bannerUpdated}>updated {updatedAgo}</span>
					</div>
				</section>

				<section className={styles.components} data-reveal="" data-reveal-delay={120}>
					<div className={styles.sectionHead}>
						<h2 className={styles.sectionTitle}>Components</h2>
						<span className={styles.sectionMeta}>
							{shownComponents[0]?.bars?.length
								? `${spanLabel(barSpan, shownComponents[0].bars.length)} of history`
								: 'no history yet'}
						</span>
					</div>
					<div className={styles.componentList}>
						{shownComponents.map((component, rowIndex) => {
							const bars = buildBars((component.bars ?? []) as HealthStatus[], component.barSpanSec ?? BASE_SPAN_SEC);
							const ongoing = isFailure(currentState(bars.map((bar) => bar.status)));
							const rowState = ongoing ? currentState(bars.map((bar) => bar.status)) : 'ok';
							const rowLabel = ongoing
								? 'ongoing incident'
								: bars.some((bar) => isFailure(bar.status))
									? 'past incident'
									: 'operational';
							return (
								<div key={component.key} className={styles.componentRow}>
									<div className={styles.componentHead}>
										<span className={`${styles.componentDot} ${styles[`componentDot_${rowState}`]}`} />
										<span className={styles.componentName}>{component.name}</span>
										<span className={styles.componentStatus}>{rowLabel}</span>
										<span className={styles.componentUptime}>
											{component.uptime} · {spanLabel(component.barSpanSec ?? BASE_SPAN_SEC, bars.length)}
										</span>
									</div>
									<div className={styles.bars}>
										{bars.map((bar, index) => (
											<Tooltip
												key={index}
												content={
													<span className={styles.tipContent}>
														<span className={[styles.tipDot, styles[`tipDot_${bar.status}`]].join(' ')} />
														{bar.label}
													</span>
												}
											>
												<span
													className={[styles.bar, styles[bar.status]].join(' ')}
													data-grow=""
													data-reveal-delay={rowIndex * 120 + index * 4}
												/>
											</Tooltip>
										))}
									</div>
								</div>
							);
						})}
						<div className={styles.axis} aria-hidden="true">
							<span>
								{shownComponents[0]?.bars?.length
									? `${spanLabel(barSpan, shownComponents[0].bars.length)} ago`
									: ''}
							</span>
							<span>{'now'}</span>
						</div>
					</div>
				</section>

				{/* Owner metrics: absent when switched off or nothing measured yet
				    (an empty grid would say the request costs nothing). */}
				{network.length > 0 && (
					<section className={styles.network} data-reveal="">
						<div className={styles.sectionHead}>
							<h2 className={styles.sectionTitle}>Network</h2>
							<span className={styles.sectionMeta}>where the time goes, median over 24 h</span>
						</div>
						<div className={styles.netGrid}>
							{network.map((check, index) => {
								const dot = watchDot(check.status);
								return (
									<div key={check.label} className={styles.netTile} data-reveal="" data-reveal-delay={index * 50}>
										<span className={styles.netLabel}>{check.label}</span>
										<span className={styles.netValue}>{check.value}</span>
										<span className={styles.netStatusRow}>
											<span
												className={styles.watchDot}
												style={{ background: dot.background, border: dot.border }}
											/>
											<span className={styles.netNote}>{check.note}</span>
										</span>
									</div>
								);
							})}
						</div>
					</section>
				)}

				{/* The incident section is no longer switchable; an empty list still
				    publishes nothing. */}
				{incidents.length > 0 && (
					<section id="history" className={styles.history} data-reveal="">
						<h2 className={styles.sectionTitle}>Incident history</h2>
						<div className={styles.incidentList}>
							{incidents.map((incident, index) => (
								<div
									key={index}
									className={`${styles.incident} ${incident.ongoing ? styles.incidentOngoing : ''}`}
								>
									<div className={styles.incidentHead}>
										<span className={styles.incidentTitle}>{incident.title}</span>
										<span className={styles.incidentDate}>{incident.since}</span>
									</div>
									<div className={styles.updates}>
										<div className={styles.update}>
											<span className={styles.updateText}>
												<span className={styles.updateState}>{incident.ongoing ? 'ongoing' : 'resolved'}</span>
											</span>
										</div>
									</div>
								</div>
							))}
						</div>
					</section>
				)}

				{poweredBy && (
					<footer className={styles.footer}>
						{/* Default on, honestly removable: AGPL means the line is a
						    default, never a lock; the switch is on the config screen. */}
						<span className={styles.poweredBy}>Powered by</span>
						<a href="https://upcontrol.io" className={styles.poweredByName} target="_blank" rel="noreferrer">
							UpControl
						</a>
						<div className={styles.spacer} />
					</footer>
				)}
			</main>
		</div>
	);
}

/** The page's own header — shared by every state so a not-found or a loading
 *  answer still looks like the site, not a blank document. */
function StatusHeader({ host }: { host: string }) {
	return (
		<header className={styles.header}>
			<div className={styles.headerInner}>
				<span className={styles.mark}>
					<BrandMark size={16} />
				</span>
				<span className={styles.title}>{host} status</span>
				<div className={styles.spacer} />
				<a href="#history" className={styles.historyLink}>
					History
				</a>
				<ThemeToggle />
			</div>
		</header>
	);
}

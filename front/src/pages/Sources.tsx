import { useState } from 'react';
import { Link } from 'react-router-dom';
import { PageHeader } from '@/components/layout';
import { Callout, IconButton, LoadError, SkeletonPanel, StatusDot, Tooltip } from '@/components/primitives';
import { SourceIcon, sourceNow } from '@/components/product';
import { CopyField } from '@/components/code';
import { CloseIcon, InfoIcon } from '@/icons';
import { invalidateApiData, useApiData } from '@/lib/useApiData';
import { incidents as incidentsApi, sources as sourcesApi, sourcesWrite } from '@/lib/client';
import styles from './Sources.module.css';

const DOT: Record<string, string> = {
	ok: 'var(--ok)',
	down: 'var(--down)',
	check: 'var(--check)',
	nodata: 'var(--nodata)',
};

// The integration guides live on the hosted docs (Decision 25d): this app
// ships no docs site of its own.
function hookDocs(kind: string): string {
	return kind === 'deployhooks'
		? 'https://upcontrol.io/docs/integrations/vercel'
		: `https://upcontrol.io/docs/integrations/${kind}`;
}

/**
 * Where the signals come from. The installer card and the project-key
 * section live on Settings — this screen is the connections and their hook
 * URLs.
 *
 * Hook laws (do not soften): looking creates nothing — opening a panel
 * fetches the URL onto a hidden draft row; COPYING is what creates the card,
 * and the first event arriving is what turns the dot green. The URL is the
 * credential: no HMAC, no self-test button — the receipt under the URL is
 * our half of the loop.
 */
export function Sources() {
	const {
		data: liveSourcesData,
		loading: sourcesLoading,
		failed: sourcesFailed,
	} = useApiData('sources', () => sourcesApi());
	const sources = liveSourcesData?.sources ?? [];
	// The tile catalogue is the server's: a kind this deployment cannot serve
	// is not offered, so a local copy of the list would lie.
	const connectableSources = liveSourcesData?.connectableSources ?? [];
	// Whether anything is open right now, from the same `incidents` cache key
	// every other screen uses — one request, one story.
	const { data: incidentsData } = useApiData('incidents', () => incidentsApi());
	const incidentOpen = (incidentsData?.items ?? []).some((item) => item.ongoing);
	// Pause is a server fact: the row's own `paused` leads, this map only holds
	// the optimistic flip until the list is re-read.
	const [paused, setPaused] = useState<Record<string, boolean>>({});
	const [sourceError, setSourceError] = useState<string | null>(null);
	/** The card asking "remove this?" — one at a time, never a native confirm. */
	const [disconnecting, setDisconnecting] = useState<string | null>(null);
	const [hookFor, setHookFor] = useState<string | null>(null);
	// A draft's token arrives through the connect response; a promoted
	// connection carries it on its listed row. The screen never invents an
	// address.
	const [freshTokens, setFreshTokens] = useState<Record<string, string>>({});

	async function refetchSources() {
		invalidateApiData('sources');
		invalidateApiData('overview');
		setPaused({});
	}

	function togglePause(id: string, next: boolean) {
		setPaused((current) => ({ ...current, [id]: next }));
		setSourceError(null);
		void sourcesWrite
			.setPaused(id, next)
			.then(refetchSources)
			.catch(() => {
				setPaused((current) => ({ ...current, [id]: !next }));
				setSourceError('That source is unchanged — pausing it failed.');
			});
	}

	// Looking creates nothing: opening the panel fetches the connection's URL
	// onto a hidden draft row. Copying re-calls this with `activate`, which is
	// what promotes the draft into the visible "waiting…" connection.
	function ensureHook(kind: string, activate = false) {
		setSourceError(null);
		void sourcesWrite
			.connect(kind, activate)
			.then((created) => {
				if (created.hookToken) {
					const token = created.hookToken;
					setFreshTokens((current) => ({ ...current, [kind]: token }));
				}
				if (activate) return refetchSources();
			})
			.catch(() => {
				setSourceError(activate ? 'That source was not connected.' : 'The hook URL could not be fetched.');
			});
	}

	function disconnectSource(id: string) {
		setDisconnecting(null);
		setSourceError(null);
		// Removing the connection kills its token, so the open instruction panel
		// (if it is this source's) folds away instead of standing there with its
		// URL just deleted out from under it.
		const kind = sources.find((source) => source.id === id)?.kind;
		if (kind) {
			setHookFor((open) => (open === kind ? null : open));
			setFreshTokens((current) => {
				const { [kind]: _dropped, ...rest } = current;
				return rest;
			});
		}
		void sourcesWrite
			.delete(id)
			.then(refetchSources)
			.catch(() => {
				setSourceError('That source is still connected — removing it failed.');
			});
	}

	// One webhook URL per provider: a connected kind's tile shows the URL
	// instead of writing a second row the server would refuse anyway.
	const connectedKinds = new Set(sources.map((source) => source.kind).filter(Boolean));
	const hookSource = hookFor ? sources.find((source) => source.kind === hookFor) : undefined;
	const hookToken = hookSource?.hookToken ?? (hookFor ? freshTokens[hookFor] : undefined);
	const hookAddress = hookToken ? `${window.location.origin}/hooks/${hookToken}` : null;

	// One header, declared once and rendered by all three branches — the whole
	// point of the shared component is that a screen does not restate its own
	// title three times.
	const header = (
		<PageHeader
			title="Sources"
			description="Where this instance gets its signal: the checks it runs itself, and anything your code or your deploys send it."
		/>
	);

	if (sourcesLoading) {
		return (
			<div className={styles.wrap}>
				{header}
				<SkeletonPanel rows={3} label="Loading sources" />
			</div>
		);
	}

	// Settled with nothing. An empty grid here reads as "nothing is connected",
	// which is the sentence that sends someone to re-run an installer they
	// have already run.
	if (sourcesFailed) {
		return (
			<div className={styles.wrap}>
				{header}
				<LoadError what="your sources" onRetry={() => invalidateApiData('sources', 'incidents')} />
			</div>
		);
	}

	return (
		<div className={styles.wrap}>
			{header}

			<div className={styles.grid}>
				{sources.map((source) => {
					const isPaused = paused[source.id] ?? source.paused;
					// The two derived sources are facts about what has arrived — there
					// is no connection to pause, and a button that answered 400 would
					// be a control that does nothing.
					const derived = source.id === 'src_checks' || source.id === 'src_logs';
					const now = sourceNow({ ...source, paused: isPaused }, incidentOpen);
					return (
						<div key={source.id} className={styles.card}>
							<SourceIcon source={source} className={styles.mark} />
							<div className={styles.cardText}>
								<span className={styles.name}>{source.name}</span>
								<span className={styles.status}>
									<span
										className={styles.statusDot}
										style={{ background: isPaused ? 'var(--text-faint)' : DOT[now.status] }}
									/>
									{now.label}
								</span>
							</div>
							{!derived && disconnecting !== source.id && (
								<span className={styles.cardActions}>
									<button type="button" className={styles.pauseButton} onClick={() => togglePause(source.id, !isPaused)}>
										{isPaused ? 'Resume' : 'Pause'}
									</button>
									<button
										type="button"
										className={styles.pauseButton}
										aria-label={`Disconnect ${source.name}`}
										onClick={() => setDisconnecting(source.id)}
									>
										Disconnect
									</button>
								</span>
							)}
							{!derived && disconnecting === source.id && (
								<span className={styles.confirmRow}>
									<button type="button" className={styles.confirmYes} onClick={() => disconnectSource(source.id)}>
										Remove
									</button>
									<button type="button" className={styles.pauseButton} onClick={() => setDisconnecting(null)}>
										Keep
									</button>
								</span>
							)}
						</div>
					);
				})}
			</div>

			<div className={styles.connectSection}>
				<span className={styles.sectionLabel}>Add a source</span>
				<div className={styles.connectGrid}>
					{connectableSources.map((source) =>
						source.installer ? (
							// The SDK installer lives on Settings in the OSS app — the
							// tile is the door there, not a second copy of the command.
							<Link key={source.key} to="/settings" className={styles.connectTile}>
								<span className={styles.connectName}>{source.name}</span>
								<span className={styles.connectTime}>{source.setupTime}</span>
							</Link>
						) : (
							// The tile opens the panel and fetches its URL; no card appears
							// from looking — the first event on the hook is what creates
							// the visible connection (the server hides the draft row).
							<button
								key={source.key}
								type="button"
								className={styles.connectTile}
								aria-pressed={connectedKinds.has(source.key)}
								onClick={() => {
									if (!connectedKinds.has(source.key)) ensureHook(source.key);
									setHookFor((open) => (open === source.key ? null : source.key));
								}}
							>
								<span className={styles.connectName}>{source.name}</span>
								<span className={styles.connectTime}>
									{connectedKinds.has(source.key) ? 'connected · show the URL' : source.setupTime}
								</span>
							</button>
						),
					)}
				</div>

				{hookFor && (
					<div className={styles.hookPanel}>
						<div className={styles.hookHead}>
							{/* Dot + word, never color alone. "Live" is the honest word:
							    the URL works from the moment it is on screen, while the
							    connection card appears once something arrives. */}
							<StatusDot status="ok" label="Live" className={styles.hookStatus} />
							<Tooltip content="How to add it" interactiveChild className={styles.hookInfoSlot}>
								<a
									className={styles.hookInfo}
									href={hookDocs(hookFor)}
									target="_blank"
									rel="noreferrer"
									aria-label={`How to add it — the ${hookFor} guide`}
								>
									<InfoIcon width={22} height={22} />
								</a>
							</Tooltip>
							<IconButton
								aria-label="Close"
								icon={<CloseIcon width={14} height={14} />}
								size="sm"
								onClick={() => setHookFor(null)}
							/>
						</div>
						<span className={styles.hookLabel}>
							Paste this URL wherever events should come from — a deploy hook, CI, a cron, anything that can
							POST JSON:
						</span>
						{hookAddress && (
							<CopyField
								text={hookAddress}
								onCopied={() => {
									if (hookFor && !hookSource) ensureHook(hookFor, true);
								}}
							/>
						)}
						{/* The receipt: what the last event was, or that none arrived yet.
						    The provider's own "send test webhook" button is the tester —
						    we never POST to ourselves. */}
						{hookAddress && (
							<span className={styles.hookReceipt}>
								{hookSource?.lastEvent ? (
									<StatusDot status="ok" label={`Received ${hookSource.lastEvent} · ${hookSource.lastSignal}`} />
								) : (
									<StatusDot status="nodata" label="Waiting for the first event…" />
								)}
							</span>
						)}
					</div>
				)}
			</div>

			{sourceError && (
				<Callout tone="danger" title="That did not save">
					{sourceError}
				</Callout>
			)}
		</div>
	);
}

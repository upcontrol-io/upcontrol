import { useEffect, useState } from 'react';
import { Link } from 'react-router-dom';
import { PageHeader } from '@/components/layout';
import { Callout, LoadError, SkeletonPanel, StatusDot } from '@/components/primitives';
import { CopyField } from '@/components/code';
import { MonitorList } from '@/components/product';
import { invalidateApiData, useApiData } from '@/lib/useApiData';
import { monitors as monitorsApi, statusPage as statusPageApi } from '@/lib/client';
import styles from './StatusPage.module.css';

/** What the owner publishes: the same list the public page renders; the
 *  "Powered by" switch is real, default on, honestly removable. */
export function StatusPage() {
	const { data: page, live, loading, failed } = useApiData('statusPage', () => statusPageApi.get());
	const { data: monitorRows, live: monitorsLive } = useApiData('monitors', () => monitorsApi.list());
	const [components, setComponents] = useState<Record<string, boolean>>({});
	const [showNetwork, setShowNetwork] = useState(true);
	const [showIncidents, setShowIncidents] = useState(true);
	const [showPoweredBy, setShowPoweredBy] = useState(true);
	const [saveError, setSaveError] = useState<string | null>(null);

	// Adopt the saved settings when they arrive.
	useEffect(() => {
		if (!page) return;
		setShowNetwork(page.showNetwork);
		setShowIncidents(page.showIncidents);
		setShowPoweredBy(page.showPoweredBy !== false);
		setComponents(Object.fromEntries(page.components.map((c) => [c.key, c.shown])));
	}, [page]);

	// The stored slug, never a formatted guess: this is an address people are
	// handed. It only renders once the read has landed.
	const publicUrl = page ? `${window.location.origin}/status/${page.slug}` : '';

	/** Every toggle saves immediately — a settings screen with no Save button
	 *  must persist on the spot, or the reader leaves believing it did. */
	function save(next: {
		shown?: Record<string, boolean>;
		showNetwork?: boolean;
		showIncidents?: boolean;
		showPoweredBy?: boolean;
	}) {
		if (!live || !page) return;
		setSaveError(null);
		void statusPageApi
			.put({
				title: page.title ?? '',
				// The stored value rides along unchanged — this screen has no
				// domain control, and dropping the field must not clear it.
				domain: page.domain ?? '',
				shown: next.shown ?? components,
				showNetwork: next.showNetwork ?? showNetwork,
				showIncidents: next.showIncidents ?? showIncidents,
				showPoweredBy: next.showPoweredBy ?? showPoweredBy,
			})
			.catch(() => setSaveError('That setting did not save. The public page is unchanged.'));
	}

	// Declared once, rendered by every branch (loading, failed, empty, live).
	const header = <PageHeader title="Status page" description="What your own visitors see. Ticked checks appear as uptime bars on the public page." />;

	if (loading) {
		return (
			<div className={styles.wrap}>
				{header}
				<SkeletonPanel rows={2} label="Loading the status page settings" />
				<SkeletonPanel rows={3} label="Loading components" />
			</div>
		);
	}

	// Settled with nothing: every switch writes on change, so a screen from
	// guesses could unpublish against unread settings..
	if (failed || !page) {
		return (
			<div className={styles.wrap}>
				{header}
				<LoadError what="your status page settings" onRetry={() => invalidateApiData('statusPage')} />
			</div>
		);
	}

	// A page with nothing on it is not a settings screen yet: the config below
	// would offer to publish a components list that does not exist.
	if (monitorsLive && (monitorRows ?? []).length === 0) {
		return (
			<div className={styles.wrap}>
				{header}
				<p className={styles.sublabel}>
					The status page publishes your checks, and there are none yet.{' '}
					<Link to="/monitors">Create the first check</Link> and it appears here as the first component.
				</p>
			</div>
		);
	}

	return (
		<div className={styles.wrap}>
			{header}

			<div className={styles.urlCard}>
				<div className={styles.urlHead}>
					{/* Dot + word, never colour alone: the ok tint on the card says
					    the same thing the label does — this address answers now. */}
					<StatusDot status="ok" label="Live" className={styles.urlStatus} />
				</div>
				<span className={styles.label}>Public URL</span>
				<div className={styles.urlRow}>
					{/* The scheme is stripped from what the eye reads, never from what
					    the clipboard gets — a pasted status link has to work. */}
					<CopyField text={publicUrl} display={publicUrl.replace(/^https?:\/\//, '')} className={styles.urlField} />
					<a href={publicUrl} target="_blank" rel="noreferrer" className={styles.visitLink}>
						Visit
					</a>
				</div>
			</div>

			{/* A component IS a check: one list, rendered once, with a publish box
			    per row. */}
			<div className={styles.componentsSection}>
				<span className={styles.label}>Components</span>
				<span className={styles.sublabel}>
					The checks this project runs. Ticked ones appear as uptime bars on the public page. Untick what
					should stay private.
				</span>
				<MonitorList
					publish={{
						shown: components,
						onToggle: (key, next) => {
							const merged = { ...components, [key]: next };
							setComponents(merged);
							save({ shown: merged });
						},
					}}
				/>
			</div>

			{saveError && (
				<Callout tone="danger" title="That did not save">
					{saveError}
				</Callout>
			)}

			<div className={styles.toggleRow}>
				<button
					type="button"
					role="switch"
					aria-checked={showNetwork}
					aria-label="Show the Network section"
					className={[styles.toggle, showNetwork && styles.toggleOn].filter(Boolean).join(' ')}
					onClick={() => {
						setShowNetwork(!showNetwork);
						save({ showNetwork: !showNetwork });
					}}
				>
					<span className={styles.toggleKnob} />
				</button>
				<span className={styles.toggleLabel}>Show the Network section, covering pings, latency and response checks</span>
			</div>

			<div className={styles.toggleRow}>
				<button
					type="button"
					role="switch"
					aria-checked={showIncidents}
					aria-label="Show past incidents"
					className={[styles.toggle, showIncidents && styles.toggleOn].filter(Boolean).join(' ')}
					onClick={() => {
						setShowIncidents(!showIncidents);
						save({ showIncidents: !showIncidents });
					}}
				>
					<span className={styles.toggleKnob} />
				</button>
				<span className={styles.toggleLabel}>Show past incidents, not just current status</span>
			</div>

			<div className={styles.toggleRow}>
				<button
					type="button"
					role="switch"
					aria-checked={showPoweredBy}
					aria-label='Show "Powered by UpControl" in the footer'
					className={[styles.toggle, showPoweredBy && styles.toggleOn].filter(Boolean).join(' ')}
					onClick={() => {
						setShowPoweredBy(!showPoweredBy);
						save({ showPoweredBy: !showPoweredBy });
					}}
				>
					<span className={styles.toggleKnob} />
				</button>
				<span className={styles.toggleLabel}>Show "Powered by UpControl" in the footer</span>
			</div>
		</div>
	);
}

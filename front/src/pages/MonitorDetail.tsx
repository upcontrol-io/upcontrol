import { useEffect, useState } from 'react';
import { Link, useNavigate, useParams } from 'react-router-dom';
import { PageHeader } from '@/components/layout';
import { Button, Callout, Input, LoadError, SkeletonPanel } from '@/components/primitives';
import type { Monitor } from '@/lib/types';
import { invalidateApiData, useApiData } from '@/lib/useApiData';
import { monitors as monitorsApi } from '@/lib/client';
import styles from './MonitorDetail.module.css';

function statusText(status: Monitor['status']): string {
	if (status === 'ok') return 'up';
	if (status === 'down') return 'down';
	return 'no data yet';
}

/** One check's facts, a rename, and the delete; no per-monitor GET exists, so
 *  the row comes from the shared list the poller keeps fresh. */
export function MonitorDetail() {
	const { id } = useParams<{ id: string }>();
	const navigate = useNavigate();
	const { loading, failed, data } = useApiData('monitors', () => monitorsApi.list());
	const monitor = (data as Monitor[] | undefined)?.find((m) => m.id === id);

	const [name, setName] = useState('');
	const [error, setError] = useState('');
	const [removing, setRemoving] = useState(false);
	// The input holds the reader's draft; the server's name seeds it once per
	// monitor, so a background poll does not type over a rename in progress.
	useEffect(() => {
		if (monitor) setName(monitor.name);
		// eslint-disable-next-line react-hooks/exhaustive-deps
	}, [monitor?.id]);

	if (loading) {
		return <SkeletonPanel rows={3} label="Loading the check" />;
	}
	if (failed) {
		return <LoadError what="this check" onRetry={() => invalidateApiData('monitors')} />;
	}
	if (!monitor) {
		// Settled, answered, and the id is not in the list: the check is gone.
		return (
			<section className={styles.page}>
				<p className={styles.gone}>This check does not exist (it may have been deleted).</p>
				<Link to="/monitors" className={styles.back}>
					Back to monitors
				</Link>
			</section>
		);
	}

	function rename() {
		const next = name.trim();
		if (!next || !monitor || next === monitor.name) return;
		void monitorsApi
			.patch(monitor.id, { name: next })
			.then(() => {
				setError('');
				invalidateApiData('monitors', 'overview', 'statusPage');
			})
			.catch((err: unknown) => {
				const message = err instanceof Error && err.message !== 'unauthorized' ? err.message : '';
				setError(message || 'Could not rename this check. Nothing was saved.');
			});
	}

	function remove() {
		if (!monitor) return;
		void monitorsApi
			.delete(monitor.id)
			.then(() => {
				invalidateApiData('monitors', 'overview', 'statusPage', 'plan');
				navigate('/monitors');
			})
			.catch(() => {
				setRemoving(false);
				setError('Could not delete this check. It is still there — try again.');
			});
	}

	const facts: [string, string][] = [
		['Type', monitor.type],
		['Target', monitor.target],
		['Status', statusText(monitor.status)],
		['Interval', monitor.interval],
	];
	if (monitor.keyword) facts.push(['Must contain', monitor.keyword]);
	if (monitor.expiry?.ssl) facts.push(['SSL', monitor.expiry.ssl]);
	if (monitor.expiry?.domain) facts.push(['Domain', monitor.expiry.domain]);

	return (
		<section className={styles.page}>
			<PageHeader title={monitor.name} back={{ to: '/monitors', label: 'Monitors' }} />

			{error && (
				<Callout tone="danger" title="That did not save">
					{error}
				</Callout>
			)}

			<dl className={styles.facts}>
				{facts.map(([label, value]) => (
					<div key={label} className={styles.fact}>
						<dt className={styles.factLabel}>{label}</dt>
						<dd className={styles.factValue}>{value}</dd>
					</div>
				))}
			</dl>

			<form
				className={styles.renameRow}
				onSubmit={(event) => {
					event.preventDefault();
					rename();
				}}
			>
				<Input label="Name" value={name} onChange={(event) => setName(event.target.value)} />
				<Button type="submit" variant="secondary" disabled={!name.trim() || name.trim() === monitor.name}>
					Rename
				</Button>
			</form>

			<div className={styles.dangerRow}>
				{removing ? (
					<div className={styles.confirmStrip}>
						<span>Stop watching this, and delete its history?</span>
						<Button variant="danger" size="sm" onClick={remove}>
							Delete
						</Button>
						<Button variant="ghost" size="sm" onClick={() => setRemoving(false)}>
							Keep
						</Button>
					</div>
				) : (
					<Button variant="secondary" size="sm" onClick={() => setRemoving(true)}>
						Delete this check
					</Button>
				)}
			</div>
		</section>
	);
}

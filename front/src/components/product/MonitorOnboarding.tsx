import { useState } from 'react';
import { Button, Callout, Checkbox, Input, SkeletonPanel } from '@/components/primitives';
import { monitors as monitorsApi, publicCheck } from '@/lib/client';
import type { components } from '@/lib/client';
import { normalizeHost } from '@/lib/normalizeTarget';
import { invalidateApiData } from '@/lib/useApiData';
import styles from './MonitorOnboarding.module.css';

type CheckResponse = components['schemas']['CheckResponse'];
type WatchRow = components['schemas']['WatchRow'];
type WatchStatus = components['schemas']['WatchStatus'];

// Status is never colour alone: the dot and the word travel together.
const STATUS_WORD: Record<WatchStatus, string> = {
	ok: 'up',
	check: 'checking',
	down: 'down',
	nodata: 'no data',
};

/** The watch rows' dot: `nodata` is an outline; nothing was measured, and a
 *  filled dot would claim a state that never ran. */
function watchDot(status: WatchStatus): { background: string; border: string } {
	if (status === 'nodata') return { background: 'transparent', border: '1px solid var(--nodata)' };
	return { background: `var(--${status})`, border: 'none' };
}

/** Which rows arrive ticked: recommended ones, group order, capped at the
 *  server's own watchLimit. */
function preTick(res: CheckResponse): Record<string, boolean> {
	const picks: Record<string, boolean> = {};
	let left = res.watchLimit ?? Number.POSITIVE_INFINITY;
	for (const group of res.groups) {
		for (const row of group.rows) {
			if (row.recommended && left > 0) {
				picks[row.id] = true;
				left -= 1;
			}
		}
	}
	return picks;
}

/** What a zero-monitor instance sees instead of an empty table: type an
 *  address, discovery scans it, Start watching creates the ticked checks. */
export function MonitorOnboarding() {
	const [host, setHost] = useState('');
	const [phase, setPhase] = useState<'idle' | 'checking' | 'results'>('idle');
	const [result, setResult] = useState<CheckResponse | null>(null);
	const [ticked, setTicked] = useState<Record<string, boolean>>({});
	const [checkFailed, setCheckFailed] = useState(false);
	const [saveFailed, setSaveFailed] = useState(false);
	const [creating, setCreating] = useState(false);

	const trimmed = normalizeHost(host);
	const rows: WatchRow[] = result ? result.groups.flatMap((group) => group.rows) : [];
	const tickedRows = rows.filter((row) => ticked[row.id]);

	function runCheck() {
		if (trimmed.length < 4) return;
		setCheckFailed(false);
		setPhase('checking');
		void publicCheck(trimmed)
			.then((res) => {
				setResult(res);
				setTicked(preTick(res));
				setPhase('results');
			})
			// Every failure is the same sentence: the reader cannot act on the
			// difference from this screen.
			.catch(() => {
				setCheckFailed(true);
				setPhase('idle');
			});
	}

	// Sequential on purpose: one refusal stops the run; no navigation, the
	// re-read flips the monitors list to its table by itself.
	async function createMonitors() {
		setCreating(true);
		setSaveFailed(false);
		try {
			for (const row of tickedRows) {
				await monitorsApi.create({
					type: 'Website',
					interval: '5m',
					target: row.id,
					name: row.id.replace(/^https?:\/\//, '').replace(/\/$/, ''),
				} as never);
			}
		} catch {
			setSaveFailed(true);
		} finally {
			setCreating(false);
			invalidateApiData('monitors', 'overview', 'statusPage', 'plan');
		}
	}

	const startLabel =
		tickedRows.length === 0
			? 'Start watching'
			: `Start watching ${tickedRows.length} ${tickedRows.length === 1 ? 'check' : 'checks'}`;

	return (
		<div className={styles.block}>
			<div className={styles.intro}>
				<h2 className={styles.heading}>Watch your first website</h2>
				<p className={styles.body}>
					Type the address of a site, <code>upcontrol.io</code> and <code>https://upcontrol.io/</code> both work,
					and it gets scanned for what is worth watching. Nothing is created until you say so.
				</p>
			</div>

			<div className={styles.addRow}>
				<div className={styles.hostCell}>
					<Input
						placeholder="example.com"
						aria-label="Website address"
						value={host}
						onChange={(event) => setHost(event.target.value)}
					/>
				</div>
				<Button
					variant="primary"
					className={styles.scanButton}
					disabled={trimmed.length < 4 || phase === 'checking'}
					onClick={runCheck}
				>
					Scan
				</Button>
			</div>

			{checkFailed && (
				<Callout tone="danger" title="Check failed">
					Could not check that address. Try again in a minute.
				</Callout>
			)}

			{phase === 'checking' && <SkeletonPanel rows={3} label="Checking the site" />}

			{phase === 'results' && result && (
				<div className={styles.results}>
					{/* One contained card, not rows floating on the page background: the
					    group bands and row fills are what make a pick-list scannable. */}
					<div className={styles.resultsCard}>
						{result.groups.map((group) => (
							<section key={group.title} className={styles.group}>
								<div className={styles.groupHead}>
									<span className={styles.groupTitle}>{group.title}</span>
									<span className={styles.groupSource}>{group.source}</span>
								</div>
								{group.rows.map((row) => {
									const dot = watchDot(row.status);
									const isTicked = Boolean(ticked[row.id]);
									return (
										// The whole row is the checkbox's own label, so every
										// pixel of the row toggles the pick.
										<Checkbox
											key={row.id}
											className={[styles.row, isTicked && styles.rowTicked].filter(Boolean).join(' ')}
											checked={isTicked}
											aria-label={`Watch ${row.name}`}
											onChange={(event) => setTicked((current) => ({ ...current, [row.id]: event.target.checked }))}
											label={
												<>
													<span className={styles.status}>
														<span className={styles.dot} style={{ background: dot.background, border: dot.border }} />
														{STATUS_WORD[row.status]}
													</span>
													<span className={styles.name}>{row.name}</span>
													<span className={styles.meta}>{row.meta}</span>
												</>
											}
										/>
									);
								})}
							</section>
						))}

						{/* The reassurance lives where the eye already is when it reaches
						    for the button: nothing here is a commitment. */}
						<div className={styles.cardFooter}>
							<p className={styles.footerNote}>
								Start with this selection. Rename, untick or add checks at any time after.
							</p>
							<Button
								variant="primary"
								className={styles.createButton}
								disabled={tickedRows.length === 0 || creating}
								onClick={() => void createMonitors()}
							>
								{startLabel}
							</Button>
						</div>
					</div>

					{saveFailed && (
						<Callout tone="danger" title="That did not save">
							Some checks could not be created. Try again.
						</Callout>
					)}
				</div>
			)}
		</div>
	);
}

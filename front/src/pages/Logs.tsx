import { PageHeader } from '@/components/layout';
import { LiveLogsPanel } from '@/components/product/LiveLogsPanel';
import styles from './Logs.module.css';

/** One logs panel: LiveLogsPanel owns the window, the filters, the timeline
 *  and Explain. The page adds the heading every other screen has — this was
 *  the one route with no `<h1>` at all, which left the panel's own small bold
 *  "Logs" doing a heading's job for a screen reader as well as for the eye. */
export function Logs() {
	return (
		<section className={styles.page}>
			<PageHeader
				title="Logs"
				description="The raw stream, newest last. Select lines and ask Explain what happened."
			/>
			<LiveLogsPanel />
		</section>
	);
}

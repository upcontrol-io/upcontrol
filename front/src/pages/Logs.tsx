import { PageHeader } from '@/components/layout';
import { LiveLogsPanel } from '@/components/product/LiveLogsPanel';
import styles from './Logs.module.css';

/** One logs panel: LiveLogsPanel owns the window, filters and timeline;
 *  the page adds the `<h1>` every other screen has. */
export function Logs() {
	return (
		<section className={styles.page}>
			<PageHeader title="Logs" description="The raw stream, newest last." />
			<LiveLogsPanel />
		</section>
	);
}

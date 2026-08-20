import { PageHeader } from '@/components/layout';
import { MonitorList } from '@/components/product';
import styles from './Monitors.module.css';

/** The checks this instance runs. The list owns create and delete; a row's
 *  name links to its detail page. */
export function Monitors() {
	return (
		<section className={styles.page}>
			<PageHeader
				title="Monitors"
				description="Every check this instance runs, and what each one last saw."
			/>
			<MonitorList />
		</section>
	);
}

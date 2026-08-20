import { BrowserRouter, Navigate, Route, Routes } from 'react-router-dom';
import { AppShell } from '@/components/layout';
import { SignIn } from '@/pages/SignIn';
import { Monitors } from '@/pages/Monitors';
import { MonitorDetail } from '@/pages/MonitorDetail';
import { Incidents } from '@/pages/Incidents';
import { IncidentDetail } from '@/pages/IncidentDetail';
import { Logs } from '@/pages/Logs';
import { Channels } from '@/pages/Channels';
import { Sources } from '@/pages/Sources';
import { StatusPage } from '@/pages/StatusPage';
import { Settings } from '@/pages/Settings';
import { PublicStatus } from '@/pages/PublicStatus';

export function App() {
	return (
		<BrowserRouter>
			<Routes>
				<Route path="/signin" element={<SignIn />} />
				{/* The public surface: no shell, no session — what a project's own
				    visitors are handed. */}
				<Route path="/status/:project" element={<PublicStatus />} />
				<Route element={<AppShell />}>
					<Route index element={<Navigate to="/monitors" replace />} />
					<Route path="/monitors" element={<Monitors />} />
					<Route path="/monitors/:id" element={<MonitorDetail />} />
					<Route path="/incidents" element={<Incidents />} />
					<Route path="/incidents/:id" element={<IncidentDetail />} />
					<Route path="/logs" element={<Logs />} />
					<Route path="/channels" element={<Channels />} />
					<Route path="/sources" element={<Sources />} />
					<Route path="/status" element={<StatusPage />} />
					<Route path="/settings" element={<Settings />} />
				</Route>
				<Route path="*" element={<Navigate to="/monitors" replace />} />
			</Routes>
		</BrowserRouter>
	);
}

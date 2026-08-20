import { StrictMode } from 'react';
import { createRoot } from 'react-dom/client';
import { App } from './App';
import { ThemeProvider } from '@/lib/theme';
import '@/styles/tokens.css';
import '@/styles/global.css';

createRoot(document.getElementById('root')!).render(
	<StrictMode>
		<ThemeProvider>
			<App />
		</ThemeProvider>
	</StrictMode>,
);

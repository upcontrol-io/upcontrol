import { useTheme } from '@/lib/theme';
import { MoonIcon, SunIcon } from '@/icons';
import { IconButton } from './IconButton';

/** Header icon-only toggle, no text label (brief §1.7). */
export function ThemeToggle() {
  const { theme, toggleTheme } = useTheme();
  return (
    <IconButton
      size="sm"
      aria-label={theme === 'dark' ? 'Switch to light theme' : 'Switch to dark theme'}
      icon={theme === 'dark' ? <SunIcon width={16} height={16} /> : <MoonIcon width={16} height={16} />}
      onClick={toggleTheme}
    />
  );
}

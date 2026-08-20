/** Shared by SignIn and the landing's watch modal — one definition of "looks like an email". */
export function validEmail(value: string): boolean {
  return /^[^\s@]+@[^\s@]+\.[^\s@]+$/.test(value);
}

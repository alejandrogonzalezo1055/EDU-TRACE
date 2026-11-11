/**
 * Origen del servicio Python (OAuth con Brightspace). El navegador solo NAVEGA a él
 * (nunca hace fetch), así que no hay CORS. Va por HTTPS en local.
 */
export const AUTH_URL = (process.env.NEXT_PUBLIC_AUTH_URL || "https://localhost:9000").replace(/\/+$/, "");

export const loginUrl = () => `${AUTH_URL}/auth/login-url`;
export const logoutUrl = () => `${AUTH_URL}/auth/logout-url`;

/** Mensajes para `/login?error=<código>` (los códigos los fija el contrato Go/Python). */
export const LOGIN_ERRORS: Record<string, string> = {
  session_expired: "Tu sesión caducó. Vuelve a entrar con Brightspace.",
  expired: "Tu sesión caducó. Vuelve a entrar con Brightspace.",
  email_unavailable: "Brightspace no nos entregó tu correo, así que no pudimos crear tu cuenta. Pide a soporte que lo revise e inténtalo de nuevo.",
  no_email: "Brightspace no nos entregó tu correo, así que no pudimos crear tu cuenta. Pide a soporte que lo revise e inténtalo de nuevo.",
  email_in_use: "Ese correo ya pertenece a una cuenta local de EduTrace. Pide al administrador que la resuelva antes de entrar con Brightspace.",
  email_conflict: "Ese correo ya pertenece a una cuenta local de EduTrace. Pide al administrador que la resuelva antes de entrar con Brightspace.",
  invalid_ticket: "El enlace de entrada no es válido o ya se usó. Vuelve a entrar con Brightspace.",
  ticket_invalid: "El enlace de entrada no es válido o ya se usó. Vuelve a entrar con Brightspace.",
  access_denied: "No autorizaste el acceso en Brightspace. Si quieres entrar, acéptalo cuando te lo pida.",
};

const FALLBACK = "No pudimos iniciar tu sesión. Inténtalo de nuevo.";

/** Texto para un `?error=`; null si no hay error. Códigos desconocidos dan el mensaje genérico. */
export function loginErrorMessage(code: string | null): string | null {
  if (!code) return null;
  return LOGIN_ERRORS[code] ?? FALLBACK;
}

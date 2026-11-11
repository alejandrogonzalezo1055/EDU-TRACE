"use client";

import { useEffect, useState } from "react";
import { useRouter } from "next/navigation";
import Link from "next/link";
import { useAuth } from "@/components/AuthProvider";
import Logo from "@/components/Logo";
import { loginErrorMessage, loginUrl } from "@/lib/auth";
import { homeFor } from "@/lib/role";

export default function LoginPage() {
  const { user, isLoading } = useAuth();
  const router = useRouter();
  const [error, setError] = useState<string | null>(null);
  const [redirecting, setRedirecting] = useState(false);

  // Go/Python devuelven aquí con /login?error=<código> cuando la entrada falla.
  useEffect(() => {
    const code = new URLSearchParams(window.location.search).get("error");
    setError(loginErrorMessage(code));
  }, []);

  useEffect(() => {
    if (!isLoading && user) router.replace(homeFor(user.role));
  }, [user, isLoading, router]);

  // Es una navegación entre dominios (redirección a Brightspace), no un fetch.
  const enter = () => {
    setRedirecting(true);
    window.location.assign(loginUrl());
  };

  return (
    <div className="relative min-h-screen overflow-hidden flex items-center justify-center p-4 sm:p-8">
      {/* Formas pastel de fondo */}
      <div aria-hidden className="pointer-events-none absolute inset-0">
        <div className="blob bg-lavender-200/70 h-[30rem] w-[30rem] -top-40 -left-32" />
        <div className="blob bg-mint-200/70 h-80 w-[28rem] top-1/3 -right-40" style={{ animationDelay: "-7s" }} />
        <div className="blob bg-peach-200/70 h-96 w-96 -bottom-48 left-1/4" style={{ animationDelay: "-14s" }} />
      </div>

      <div className="relative grid w-full max-w-5xl rounded-[2rem] bg-white/60 shadow-lift backdrop-blur-xl md:grid-cols-2">
        {/* Presentación */}
        <div className="relative hidden md:flex flex-col justify-center gap-10 rounded-l-[2rem] bg-lavender-100 p-10 lg:p-12">
          <div aria-hidden className="pointer-events-none absolute inset-0 overflow-hidden rounded-l-[2rem]">
            <div className="blob bg-mint-200 h-64 w-80 -bottom-16 -right-20" />
            <div className="blob bg-peach-200/80 h-40 w-40 top-24 -right-10" style={{ animationDelay: "-5s" }} />
          </div>

          <div className="relative rise-in">
            <Link href="/inicio" aria-label="EduTrace, inicio" className="mb-8 flex items-center gap-3">
              <Logo full className="h-32 w-auto" />
            </Link>
            <h1 className="text-4xl font-extrabold leading-tight tracking-tight">
              Programa. Comprende. <span className="text-lavender-600">Avanza.</span>
            </h1>
            <p className="mt-3 max-w-sm text-lg font-semibold leading-snug text-ink">
              Practica, aprende de tus errores y recibe orientación paso a paso. Tu docente acompaña tu progreso.
            </p>
            <p className="mt-3 max-w-sm text-ink-soft">
              Escribe y compila C++ desde el navegador, comprueba tu programa con casos de prueba y entiende cada error. Recibe pistas de
              IA y el acompañamiento de tu docente en cada paso.
            </p>
            <p className="mt-4 text-sm font-bold text-lavender-700">De javerianos para javerianos.</p>
          </div>
        </div>

        {/* Entrada con Brightspace */}
        <div className="relative rounded-[2rem] bg-white p-7 sm:p-10 md:rounded-l-none">
          <Link href="/inicio" aria-label="EduTrace, inicio" className="md:hidden mb-6 flex items-center gap-2">
            <Logo full className="h-16 w-auto" />
          </Link>

          <div className="rise-in">
            <h2 className="text-2xl font-extrabold tracking-tight">¡Hola!</h2>
            <p className="mt-1 mb-6 text-sm text-ink-soft">
              Entra con tu cuenta de Brightspace. Tus clases, tu rol y tus talleres vienen de allí, así que no necesitas crear otra cuenta
              ni otra contraseña.
            </p>

            {error && (
              <p role="alert" className="mb-4 rounded-2xl bg-peach-50 px-4 py-2.5 text-sm font-semibold text-peach-700">
                {error}
              </p>
            )}

            <button
              type="button"
              onClick={enter}
              disabled={redirecting}
              className="w-full rounded-full bg-lavender-600 py-3 font-bold text-white shadow-soft transition
                hover:bg-lavender-700 hover:shadow-lift active:scale-[0.99] disabled:opacity-50"
            >
              {redirecting ? "Llevándote a Brightspace..." : "Entrar con Brightspace"}
            </button>

            <p className="mt-4 text-xs leading-relaxed text-ink-faint">
              La primera vez te pediremos tu autorización para el tratamiento de datos (Ley 1581 de 2012). Puedes leer la{" "}
              <Link href="/privacidad" target="_blank" className="font-bold text-lavender-700 underline">
                política de tratamiento de datos
              </Link>
              .
            </p>
            <p className="mt-5 text-center text-sm">
              <Link href="/inicio" className="font-semibold text-ink-soft hover:text-lavender-700">
                Volver al inicio
              </Link>
            </p>
          </div>
        </div>
      </div>
    </div>
  );
}

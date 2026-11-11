"use client";

import { StudentGroup } from "@/lib/api";

const CLASS_COLORS = [
  { on: "bg-lavender-600 text-white", sub: "text-lavender-100", off: "bg-lavender-100 text-ink hover:bg-lavender-200" },
  { on: "bg-mint-700 text-white", sub: "text-mint-100", off: "bg-mint-100 text-ink hover:bg-mint-200" },
  { on: "bg-peach-700 text-white", sub: "text-peach-100", off: "bg-peach-100 text-ink hover:bg-peach-200" },
  { on: "bg-sky-700 text-white", sub: "text-sky-100", off: "bg-sky-100 text-ink hover:bg-sky-200" },
  { on: "bg-rose-700 text-white", sub: "text-rose-100", off: "bg-rose-100 text-ink hover:bg-rose-200" },
  { on: "bg-amber-800 text-white", sub: "text-amber-100", off: "bg-amber-100 text-ink hover:bg-amber-200" },
];

/** Clases del estudiante (vienen de Brightspace): elegir a cuál enviar. */
export default function StudentClasses({
  groups,
  selectedId,
  onSelect,
}: {
  groups: StudentGroup[];
  selectedId: string | null;
  onSelect: (id: string) => void;
}) {
  return (
    <div className="flex flex-wrap items-start justify-between gap-4 border-t border-lavender-100 pt-4">
      <div className="min-w-0 flex-1">
        <p className="mb-2 text-xs font-bold uppercase tracking-wider text-ink-faint">
          {groups.length ? "Enviar a la clase" : "Mis clases"}
        </p>
        {groups.length === 0 ? (
          <p className="text-sm text-ink-soft">
            Todavía no apareces inscrito en ninguna clase. Tus clases se gestionan en Brightspace.
          </p>
        ) : (
          <div role="radiogroup" aria-label="Clase a la que envías" className="flex flex-wrap gap-2">
            {groups.map((g, i) => {
              const color = CLASS_COLORS[i % CLASS_COLORS.length];
              return (
                <button
                  key={g.id}
                  type="button"
                  role="radio"
                  aria-checked={selectedId === g.id}
                  onClick={() => onSelect(g.id)}
                  className={`rounded-2xl px-3.5 py-2 text-left transition ${selectedId === g.id ? `${color.on} shadow-soft` : color.off}`}
                >
                  <span className="block text-sm font-bold">{g.course_name}</span>
                  <span className={`block text-xs ${selectedId === g.id ? color.sub : "text-ink"}`}>
                    {g.name} con {g.teacher_name}
                  </span>
                </button>
              );
            })}
          </div>
        )}
        {groups.length > 0 && (
          <p className="mt-2 max-w-xl text-xs text-ink-faint">
            Tus compilaciones en la clase elegida se guardan como <strong>intentos</strong> para que tu profesor vea tu
            proceso. Solo lo que mandas con <strong>Enviar</strong> es tu entrega oficial.
          </p>
        )}
      </div>

      <p className="w-full text-xs text-ink-faint sm:w-auto sm:max-w-[16rem]">
        Tus clases se gestionan en Brightspace. Si falta alguna, inscríbete allá y vuelve a entrar.
      </p>
    </div>
  );
}

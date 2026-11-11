"use client";

import { BrightspaceStatus, FeedbackOrigin, StudentSubmission, SubmissionMeta } from "@/lib/api";
import { MonitorNotes } from "@/components/MonitorFeedback";
import ComprehensionCheck from "@/components/ComprehensionCheck";
import { StudentQuestions } from "@/components/FeedbackQuestions";
import { formatDateTime, formatFeedback, LEVEL_STYLE, performanceBand } from "@/lib/format";

const BRIGHTSPACE_PILL: Record<BrightspaceStatus, { label: string; style: string }> = {
  pendiente: { label: "Enviando a Brightspace", style: "bg-amber-50 text-amber-800" },
  enviada: { label: "Enviada a Brightspace", style: "bg-mint-100 text-mint-700" },
  fallida: { label: "No se pudo enviar a Brightspace", style: "bg-peach-50 text-peach-700" },
};

/** Estado del envío oficial a Brightspace; no pinta nada si el backend aún no manda el campo. */
export function BrightspacePill({ meta }: { meta: Pick<SubmissionMeta, "kind" | "brightspace_status" | "brightspace_error"> }) {
  const st = meta.brightspace_status;
  if (!st || !BRIGHTSPACE_PILL[st]) return null;
  const { label, style } = BRIGHTSPACE_PILL[st];
  return (
    <span title={st === "fallida" ? meta.brightspace_error : undefined} className={`rounded-full px-3 py-1 text-xs font-bold ${style}`}>
      {label}
    </span>
  );
}

export function ReviewStatusPill({ item }: { item: StudentSubmission }) {
  return item.review ? (
    <span className="rounded-full bg-mint-100 px-3 py-1 text-xs font-bold text-mint-700">
      Revisada
      {item.review.grade !== undefined ? `: ${item.review.grade.toFixed(1)} / 5.0, ${performanceBand(item.review.grade).label}` : ""}
    </span>
  ) : (
    <span className="rounded-full bg-white px-3 py-1 text-xs font-bold text-ink-soft">En revisión</span>
  );
}

export function MonitorCountPill({ item }: { item: StudentSubmission }) {
  const n = item.monitor_feedback?.length ?? 0;
  if (n === 0) return null;
  return (
    <span className="rounded-full bg-mint-50 px-3 py-1 text-xs font-bold text-mint-700">
      {n === 1 ? "Comentario del monitor" : `${n} comentarios del monitor`}
    </span>
  );
}

export function AnnotationsPill({ item }: { item: StudentSubmission }) {
  const n = item.monitor_annotations ?? 0;
  if (n === 0) return null;
  return (
    <span className="rounded-full bg-lavender-100 px-3 py-1 text-xs font-bold text-lavender-700">
      {n === 1 ? "1 marca en tu código" : `${n} marcas en tu código`}
    </span>
  );
}

const ORIGIN_NOTE: Record<FeedbackOrigin, string> = {
  ia: "Texto propuesto por la IA y aprobado sin cambios por",
  ia_editada: "Texto propuesto por la IA, editado y aprobado por",
  docente: "Retroalimentación escrita por",
};

export function SubmissionFeedback({ item }: { item: StudentSubmission }) {
  const review = item.review;
  const origin = review?.origin || (review?.ai_assisted ? "ia_editada" : "docente");
  const teacher = review?.teacher_name || "tu profesor";
  return (
    <div className="space-y-3">
      {item.brightspace_status === "fallida" && (
        <p role="status" className="rounded-2xl bg-peach-50 px-4 py-2 text-sm font-semibold text-peach-700">
          Tu entrega quedó guardada en EduTrace, pero no llegó a Brightspace{item.brightspace_error ? `: ${item.brightspace_error}` : ""}. Si el
          problema continúa, vuelve a entrar con Brightspace y envíala de nuevo.
        </p>
      )}
      {item.brightspace_status === "pendiente" && (
        <p role="status" className="rounded-2xl bg-amber-50 px-4 py-2 text-sm font-semibold text-amber-900">
          Tu entrega está guardada y se está enviando a Brightspace. EduTrace lo reintentará solo.
        </p>
      )}
      {review ? (
        <div>
          <div className="mb-2 flex flex-wrap items-center gap-2">
            {review.brightspace_published && (
              <span className="rounded-full bg-mint-100 px-2.5 py-0.5 text-[11px] font-bold text-mint-700">Publicada en Brightspace</span>
            )}
            {origin !== "docente" ? (
              <span className="rounded-full bg-lavender-100 px-2.5 py-0.5 text-[11px] font-bold text-lavender-700">
                Generado con IA y aprobado por {teacher}
              </span>
            ) : (
              <span className="rounded-full bg-mint-100 px-2.5 py-0.5 text-[11px] font-bold text-mint-700">Escrito por {teacher}</span>
            )}
          </div>
          <div
            className="rounded-2xl bg-white p-4 text-sm leading-relaxed"
            dangerouslySetInnerHTML={{ __html: formatFeedback(review.feedback) }}
          />
          {review.raes && review.raes.length > 0 && (
            <ul className="mt-2 space-y-1.5 rounded-2xl bg-white p-3">
              <li className="text-xs font-bold uppercase tracking-wider text-ink-soft">Nivel de logro por RAE</li>
              {review.raes.map((r) => (
                <li key={r.code} className="flex items-start gap-2 text-sm">
                  <span className={`shrink-0 rounded-full px-2.5 py-0.5 text-[11px] font-bold capitalize ${LEVEL_STYLE[r.level]}`}>
                    {r.level}
                  </span>
                  <span>
                    <span className="font-bold">{r.code}</span> <span className="text-ink-soft">{r.description}</span>
                  </span>
                </li>
              ))}
            </ul>
          )}
          {(review.criteria ?? []).length > 0 && (
            <table className="mt-2 w-full rounded-2xl bg-white text-left text-sm">
              <caption className="px-3 pt-2 text-left text-xs font-bold uppercase tracking-wider text-ink-soft">Rúbrica</caption>
              <tbody>
                {review.criteria!.map((c) => (
                  <tr key={c.id} className="border-t border-lavender-50">
                    <th scope="row" className="px-3 py-1.5 font-normal">{c.name}</th>
                    <td className="px-3 py-1.5 text-right font-bold tabular-nums">
                      {c.score !== undefined ? c.score.toFixed(1) : "—"} / {c.points.toFixed(1)}
                    </td>
                  </tr>
                ))}
              </tbody>
            </table>
          )}
          <p className="mt-2 text-xs text-ink-faint">
            {ORIGIN_NOTE[origin]} {teacher} el {formatDateTime(review.returned_at)}.
            {review.ai_assisted && " La calificación la definió tu profesor, no la IA."} Si tienes dudas sobre esta retroalimentación,
            pregúntale a {teacher} aquí abajo o a tu monitor.
          </p>
          <div className="mt-2">
            <ComprehensionCheck kind="revision" submissionId={item.id} initial={review.understood} />
          </div>
        </div>
      ) : (
        <p className="text-sm text-ink-soft">Tu profesor aún no ha revisado esta entrega.</p>
      )}
      {(review || item.has_ai_feedback || (item.questions?.length ?? 0) > 0) && (
        <StudentQuestions submissionId={item.id} initial={item.questions ?? []} />
      )}
      {(item.monitor_feedback?.length ?? 0) > 0 && (
        <div className="space-y-2">
          <p className="text-xs font-bold uppercase tracking-wider text-ink-soft">Retroalimentación de tu monitor</p>
          <MonitorNotes notes={item.monitor_feedback!} />
        </div>
      )}
    </div>
  );
}

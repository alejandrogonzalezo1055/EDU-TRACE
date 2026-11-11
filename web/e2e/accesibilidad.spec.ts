import { APIRequestContext, Browser, expect, Page, request, test } from "@playwright/test";
import AxeBuilder from "@axe-core/playwright";

const API = process.env.E2E_API ?? "http://localhost:8080";
// Ya no hay registro ni contraseñas: los datos entran por /internal/v1 (como los empujaría el servicio Python)
// y la sesión se abre canjeando un ticket de un solo uso. Es la INTERNAL_API_KEY con la que corre el backend.
const INTERNAL_KEY = process.env.E2E_INTERNAL_API_KEY ?? "";
const RUN = Date.now();
const TAGS = ["wcag2a", "wcag2aa", "wcag21a", "wcag21aa"];

type Session = { api: APIRequestContext; email: string; id: string; ext: string; groups: Record<string, string> };

let teacher: Session;
let student: Session;
let monitor: Session;
let groupId = "";

async function call<T>(api: APIRequestContext, method: string, path: string, data?: unknown): Promise<T> {
  const response = await api.fetch(`${API}${path}`, { method, data });
  expect(response.ok(), `${method} ${path}: ${response.status()} ${await response.text()}`).toBeTruthy();
  return response.status() === 204 ? (undefined as T) : response.json();
}

const internalHeaders = () => ({ Authorization: `Bearer ${INTERNAL_KEY}`, "Content-Type": "application/json" });

async function internal<T>(api: APIRequestContext, method: string, path: string, data: unknown): Promise<T> {
  const response = await api.fetch(`${API}/internal/v1/${path}`, { method, data, headers: internalHeaders() });
  expect(response.ok(), `${method} ${path}: ${response.status()} ${await response.text()}`).toBeTruthy();
  return response.json();
}

type Course = { external_id: string; name: string };

// Sincroniza al usuario y sus inscripciones como lo haría Python, y abre su sesión con un ticket.
async function account(role: "teacher" | "student", name: string, courses: Course[], seq: number): Promise<Session> {
  const api = await request.newContext();
  const ext = `d2l:user:${RUN}${seq}`;
  const email = `${role}-${RUN}-${seq}@prueba.edu`;
  const user = await internal<{ id: string }>(api, "PUT", `users/${ext}`, { name, email });
  const enrolled = await internal<{ groups: { course_external_id: string; group_id: string }[] }>(api, "PUT", `users/${ext}/enrollments`, {
    enrollments: courses.map((course) => ({ course: { ...course, is_active: true }, role })),
  });
  const { ticket } = await internal<{ ticket: string }>(api, "POST", "session-tickets", { user_external_id: ext });
  // El backend responde 302 con Set-Cookie: sin seguir la redirección, la cookie queda en el contexto.
  await api.fetch(`${API}/api/auth/session?ticket=${ticket}`, { maxRedirects: 0 });
  await call(api, "POST", "/api/auth/consent");
  return { api, email, id: user.id, ext, groups: Object.fromEntries(enrolled.groups.map((g) => [g.course_external_id, g.group_id])) };
}

async function pageAs(browser: Browser, session: Session): Promise<Page> {
  const context = await browser.newContext({ storageState: await session.api.storageState() });
  return context.newPage();
}

async function expectAccessible(page: Page, label: string) {
  await page.waitForLoadState("networkidle");
  const { violations } = await new AxeBuilder({ page }).withTags(TAGS).analyze();
  const report = violations.map(
    (v) => `${v.id} (${v.impact}): ${v.help} → ${v.nodes.slice(0, 4).map((n) => n.target.join(" ")).join(" | ")}`
  );
  expect(report, `${label}: problemas de accesibilidad`).toEqual([]);
}

test.beforeAll(async () => {
  expect(INTERNAL_KEY, "falta E2E_INTERNAL_API_KEY (la INTERNAL_API_KEY del backend)").not.toBe("");
  // Cada oferta de Brightspace es un curso con su clase.
  const first: Course = { external_id: `d2l:ou:${RUN}1`, name: "Programación I" };
  const second: Course = { external_id: `d2l:ou:${RUN}2`, name: "Programación II" };
  teacher = await account("teacher", "Elena Prueba", [first, second], 1);
  student = await account("student", "Ana Prueba", [first], 2);
  // El monitor es un estudiante de otra materia al que el docente asigna como monitor (concepto local de EduTrace).
  monitor = await account("student", "Mona Prueba", [second], 3);
  groupId = teacher.groups[first.external_id];
  await call(teacher.api, "POST", `/api/teacher/groups/${groupId}/monitors`, { email: monitor.email });
  // Los talleres son carpetas de entrega de Brightspace; rúbrica y casos de prueba los pone el docente.
  const due = new Date(Date.now() + 7 * 24 * 3600 * 1000).toISOString();
  const title = "Taller 1: suma";
  const statement = "Lee n e imprime la suma de 1 a n.";
  await internal(teacher.api, "PUT", `courses/${first.external_id}/assignments`, {
    assignments: [{ external_id: `d2l:folder:${RUN}1`, title, statement, due_at: due, end_at: due, score_denominator: 100, is_hidden: false }],
  });
  const tests = JSON.stringify([
    { name: "Suma", stdin: "3", expected: "6", match: "contiene", hidden: false },
    { name: "Oculto", stdin: "4", expected: "10", match: "contiene", hidden: true },
  ]);
  const folders = await call<{ id: string; title: string }[]>(teacher.api, "GET", `/api/teacher/groups/${groupId}/assignments`);
  const folder = folders.find((a) => a.title === title);
  expect(folder, "carpeta sincronizada").toBeTruthy();
  const created = await teacher.api.fetch(`${API}/api/teacher/assignments/${folder!.id}`, {
    method: "PUT",
    multipart: {
      title,
      statement,
      tests,
      criteria: JSON.stringify([
        { name: "Compila", points: 1 },
        { name: "Resultado correcto", points: 4 },
      ]),
    },
  });
  expect(created.ok(), await created.text()).toBeTruthy();
  const assignment = await created.json();
  const code = "#include <iostream>\nint main() {\n  int n, s = 0;\n  std::cin >> n;\n  for (int i = 1; i <= n; i++) s += i;\n  std::cout << s << std::endl;\n}\n";
  const sub = await call<{ id: string }>(student.api, "POST", "/api/student/submissions", {
    group_id: groupId,
    assignment_id: assignment.id,
    code,
    stdin: "3",
  });
  await call(teacher.api, "PUT", `/api/teacher/submissions/${sub.id}/review`, {
    feedback: "## ¿Hacia dónde voy?\nSumar de 1 a n.\n\n## ¿Cómo voy?\nFunciona.\n\n## ¿Qué sigue?\nPrueba con n = 0.",
    grade: 4.5,
    return: true,
  });
  await call(student.api, "POST", `/api/student/submissions/${sub.id}/questions`, { body: "¿Qué pasa si n es negativo?" });
});

test("páginas públicas", async ({ page }) => {
  for (const path of ["/inicio", "/login", "/login?error=invalid_ticket", "/privacidad"]) {
    await page.goto(path);
    await expectAccessible(page, path);
  }
});

test("estudiante", async ({ browser }) => {
  const page = await pageAs(browser, student);
  for (const path of ["/", "/historial", "/cuenta", "/practica", "/progreso", "/?ejercicio=suma-1-n"]) {
    await page.goto(path);
    await expect(page.getByRole("heading", { level: 1 })).toBeVisible();
    await expectAccessible(page, path);
  }
  await page.goto("/");
  await page.getByRole("button", { name: /^Avisos/ }).click();
  await expect(page.getByRole("heading", { name: "Avisos" })).toBeVisible();
  await expectAccessible(page, "/ con los avisos abiertos");
});

test("profesor", async ({ browser }) => {
  const page = await pageAs(browser, teacher);
  for (const path of ["/docente", "/docente/historial", "/docente/progreso", `/docente/clases/${groupId}`, `/docente/clases/${groupId}/reporte/${student.id}`, "/cuenta"]) {
    await page.goto(path);
    await expect(page.getByRole("heading", { level: 1 }).first()).toBeVisible();
    await expectAccessible(page, path);
  }
  await page.goto(`/docente/clases/${groupId}`);
  await page.getByRole("button", { name: /Ana Prueba.*Taller 1: suma/ }).click();
  await expect(page.getByText("Rúbrica").first()).toBeVisible();
  await expectAccessible(page, "revisión con rúbrica");
  await page.getByRole("combobox", { name: "Buscar en EduTrace" }).fill("ana");
  await expect(page.getByRole("option", { name: /Ana Prueba/ })).toBeVisible();
  await expectAccessible(page, "barra de búsqueda con resultados");
});

test("monitor", async ({ browser }) => {
  const page = await pageAs(browser, monitor);
  for (const path of ["/monitor", `/monitor/clases/${groupId}`, "/", "/historial", "/practica", "/progreso"]) {
    await page.goto(path);
    await expect(page.getByRole("heading", { level: 1 }).first()).toBeVisible();
    await expectAccessible(page, path);
  }
});

test.afterAll(async () => {
  await teacher?.api.dispose();
  await student?.api.dispose();
  await monitor?.api.dispose();
});

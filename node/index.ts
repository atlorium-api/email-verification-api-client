/**
 * Клиент API «Проверка почты» Atlorium — существует ли ящик, не спам-ловушка ли это.
 *
 * Запуск (работает сразу, без регистрации — на демо-ключе):
 *   npm install
 *   npm start
 *   npm start -- "anna@example.com,promo@spamtrap.example.com"
 *
 * Боевой ключ: получить на https://atlorium.com и положить в переменную окружения
 * ATLORIUM_API_KEY. Код при этом не меняется.
 */

/**
 * Публичный демо-ключ. С ним API отвечает правдоподобными МОКАМИ (не реальными
 * данными) — чтобы можно было встроить и протестировать интеграцию до оплаты.
 * Ответы детерминированы: один и тот же адрес всегда даёт один и тот же результат,
 * поэтому на них можно писать стабильные тесты.
 */
const SANDBOX_KEY = 'ak_sandbox_demo_mockdata_v1';

const API_KEY = process.env.ATLORIUM_API_KEY ?? SANDBOX_KEY;
const BASE_URL = process.env.ATLORIUM_BASE_URL ?? 'https://atlorium.com';

// Проверка ящика — синхронное обращение к почтовому серверу домена, он может тянуть
// с ответом. Отсюда таймаут заметно больше, чем у справочников.
const TIMEOUT_MS = 30_000;

// Проверка почты — платный сервис, лимит жёсткий.
// Список рассылки длиннее двух адресов гарантированно упрётся в 429 — поэтому
// повтор после паузы здесь не «на всякий случай», а штатный режим работы.
const RETRY_DELAY_MS = 30_000;
const MAX_RETRIES = 3;

// Потолок ожидания. Исчерпав ЧАСОВОЙ лимит, сервер честно отвечает Retry-After на
// 40+ минут — и клиент, слепо доверяющий заголовку, «зависнет» на эти 40 минут
// (а в CI просто съест весь бюджет джоба). Дольше потолка не ждём.
const MAX_RETRY_DELAY_MS = 120_000;

/** Итоговый вердикт по адресу. Ровно столько значений, сколько различает источник. */
export type EmailStatus =
  | 'Valid'
  | 'Invalid'
  | 'CatchAll'
  | 'Unknown'
  | 'SpamTrap'
  | 'Abuse'
  | 'DoNotMail';

/** Карточка проверки одного адреса. */
export interface EmailReport {
  email: string;
  status: EmailStatus;
  /** Статус ровно в том виде, как его вернул источник: "valid", "catch-all", "do_not_mail". */
  statusRaw: string;
  /** Уточнение: "mailbox_not_found", "disposable", "role_based", "greylisted". */
  subStatus: string | null;
  account: string | null;
  domain: string | null;
  freeEmail: boolean;
  catchAllDomain: boolean | null;
  /** Подсказка по опечатке в домене: gmial.com → gmail.com. */
  didYouMean: string | null;
  domainAgeDays: number | null;
  smtpProvider: string | null;
  mxFound: boolean;
  mxRecord: string | null;
  activeInDays: string | null;
  activeFirstSeen: string | null;
  processedAtUtc: string | null;
  elapsedMs: number;
  /** true только при status = Valid. catch-all и unknown — неопределённость. */
  deliverable: boolean;
}

const ERROR_REASONS: Record<number, string> = {
  400: 'Адрес не передан или синтаксически некорректен (запрос НЕ тарифицируется)',
  401: 'API-ключ отсутствует, просрочен или недействителен',
  402: 'Недостаточно кредитов на балансе — пополните на https://atlorium.com',
  429: 'Превышен лимит запросов — повторите позже',
  503: 'Сервис проверки почты временно недоступен (за сбой на своей стороне мы не списываем деньги)',
};

/** Ошибка API: HTTP-код разложен в человекочитаемую причину. */
export class AtloriumError extends Error {
  constructor(readonly status: number, body: string) {
    const reason = ERROR_REASONS[status] ?? 'Неизвестная ошибка';
    super(`HTTP ${status}: ${reason}. Ответ сервера: ${body.slice(0, 200)}`);
    this.name = 'AtloriumError';
  }
}

const sleep = (ms: number): Promise<void> => new Promise((resolve) => setTimeout(resolve, ms));

/**
 * Сколько ждать после 429. Ноль/мусор и слишком большие значения не берём на веру.
 *
 * Значение 0 (или мусор) означало бы «повторяй немедленно» — клиент ушёл бы в
 * busy-loop и выжег остаток лимита за секунду. Значение в 40+ минут (так сервер
 * отвечает на исчерпанный часовой лимит) означало бы «спи почти час» — этого мы
 * тоже не делаем. Возвращаем 0, если ждать бессмысленно долго: вызывающий сдастся.
 */
function retryAfterMs(response: Response): number {
  const seconds = Number(response.headers.get('Retry-After'));
  if (!Number.isFinite(seconds) || seconds <= 0) {
    return RETRY_DELAY_MS;
  }
  const ms = seconds * 1000;
  return ms <= MAX_RETRY_DELAY_MS ? ms : 0;
}

/**
 * Проверка одного адреса: GET /api/EmailValidation?email=...
 *
 * Письмо на адрес НЕ отправляется: провайдер обращается к почтовому серверу домена
 * и выясняет, принял бы тот письмо на этот ящик.
 */
export async function verifyEmail(email: string): Promise<EmailReport> {
  const url = new URL('/api/EmailValidation', BASE_URL);
  url.searchParams.set('email', email);

  for (let attempt = 0; ; attempt++) {
    const response = await fetch(url, {
      headers: {
        Authorization: `Bearer ${API_KEY}`,
        Accept: 'application/json',
      },
      signal: AbortSignal.timeout(TIMEOUT_MS),
    });

    // 429 — не поломка, а реальный лимит продукта. Ждём и повторяем.
    if (response.status === 429 && attempt < MAX_RETRIES) {
      const delay = retryAfterMs(response);
      if (delay === 0) {
        // Сервер просит ждать дольше потолка — значит, исчерпан часовой лимит.
        // Спать 40 минут бессмысленно: честно говорим об этом и выходим.
        throw new AtloriumError(429, 'лимит запросов по IP исчерпан, повторите позже');
      }
      console.error(`  ... лимит запросов, пауза ${delay / 1000} с`);
      await sleep(delay);
      continue;
    }

    if (!response.ok) {
      throw new AtloriumError(response.status, await response.text());
    }
    return (await response.json()) as EmailReport;
  }
}

// ── Применение данных: чистка списка рассылки ─────────────────────────────────
// Отчёт по адресу сам по себе — просто JSON. Ценность появляется, когда по нему
// принимают решение: слать письмо или нет. Ниже — ровно это решение.

export type Action =
  | 'SEND'         // отправляем
  | 'SEND_RISKY'   // можно отправить, но на свой риск
  | 'RETRY_LATER'  // проверить не удалось, повторить позже
  | 'DROP';        // из списка выбросить

export interface Decision {
  email: string;
  statusRaw: string;
  action: Action;
  reason: string;
  /**
   * Спам-ловушка вынесена в отдельный флаг: это не «ещё один DROP», а причина
   * остановить рассылку и разобраться, откуда взялся адрес.
   */
  spamtrap: boolean;
  /** Подсказка по опечатке в домене: gmial.com → gmail.com. */
  didYouMean: string | null;
}

/** Что делать с адресом, исходя из вердикта проверки. */
export function decide(report: EmailReport): Decision {
  const base = {
    email: report.email,
    statusRaw: report.statusRaw,
    spamtrap: false,
    didYouMean: report.didYouMean,
  };

  switch (report.status) {
    case 'Valid':
      return { ...base, action: 'SEND', reason: 'Ящик существует, домен принимает почту' };

    case 'CatchAll':
      // Домен отвечает «принято» на ЛЮБОЙ адрес, поэтому существование конкретного
      // ящика проверить снаружи невозможно. Это не «плохо» — это неопределённость.
      // Слать можно, но отдельным сегментом и следя за bounce rate.
      return {
        ...base,
        action: 'SEND_RISKY',
        reason: 'Домен принимает всё подряд — доставляемость не гарантирована',
      };

    case 'Unknown':
      // Сервер домена применил greylisting, не ответил или заблокировал проверку.
      // За такой исход деньги НЕ списываются — адрес просто проверяем позже.
      return {
        ...base,
        action: 'RETRY_LATER',
        reason: 'Сервер домена не ответил (greylisting) — запрос не тарифицирован',
      };

    case 'Invalid':
      // Ящика не существует. Отправка = hard bounce, а bounce rate выше 2 %
      // почтовые провайдеры считают признаком грязной базы.
      return {
        ...base,
        action: 'DROP',
        reason: 'Ящика не существует — письмо отскочит (hard bounce)',
      };

    case 'DoNotMail': {
      // Технически ящик может работать, но рассылать на него нельзя.
      const reason =
        report.subStatus === 'role_based'
          ? 'Ролевой адрес (info@, sales@) — читает не человек, а отдел'
          : report.subStatus === 'disposable'
            ? 'Одноразовый ящик — создан на 10 минут, писать бессмысленно'
            : 'Адрес из списка «не писать»';
      return { ...base, action: 'DROP', reason };
    }

    case 'Abuse':
      // Владелец адреса уже жаловался на спам. Ещё одна жалоба — минус к репутации.
      return { ...base, action: 'DROP', reason: 'Жалобщик: ранее отмечал письма как спам' };

    case 'SpamTrap':
      // САМОЕ ВАЖНОЕ, ЧТО ДАЁТ СЕРВИС.
      // Спам-ловушка — адрес, который никто не заводил и на который никто не
      // подписывался: его специально публикуют, чтобы поймать тех, кто рассылает
      // по купленным и спарсенным базам. Ящик отвечает как живой, поэтому отличить
      // его самому невозможно. Одно попадание — и репутация домена-отправителя
      // падает, а в спам уходит ВСЯ рассылка, включая письма живым клиентам.
      return {
        ...base,
        action: 'DROP',
        reason: 'СПАМ-ЛОВУШКА — убивает репутацию домена-отправителя',
        spamtrap: true,
      };

    default:
      // Провайдер вернул статус, которого мы не знаем. Не рискуем.
      return { ...base, action: 'DROP', reason: `Неизвестный статус: ${report.statusRaw}` };
  }
}

export interface Summary {
  decisions: Decision[];
  spamtraps: Decision[];
  typos: Decision[];
  invalid: number;
  /** Какой был бы bounce rate, если бы список отправили как есть. */
  bounceRate: number;
}

/** Чистка списка рассылки: каждый адрес проверяется и получает решение. */
export async function filterMailingList(emails: string[]): Promise<Summary> {
  const decisions: Decision[] = [];
  for (const email of emails) {
    decisions.push(decide(await verifyEmail(email)));
  }

  const invalid = decisions.filter((d) => d.statusRaw === 'invalid').length;

  return {
    decisions,
    spamtraps: decisions.filter((d) => d.spamtrap),
    typos: decisions.filter((d) => d.didYouMean),
    invalid,
    bounceRate: decisions.length > 0 ? (100 * invalid) / decisions.length : 0,
  };
}

const pad = (value: string, width: number): string => value.padEnd(width);

async function main(): Promise<void> {
  if (API_KEY === SANDBOX_KEY) {
    console.log('Демо-ключ: ответы сгенерированы (моки), не реальные данные.\n');
  }

  // Список короткий намеренно: проверка почты — платный сервис с жёстким лимитом.
  // Домены ниже — сценарии песочницы, см. README.
  const defaultList = [
    'anna.petrova@example.com',   // обычный живой ящик
    'ivan@invalid.example.com',   // ящика не существует
    'promo@spamtrap.example.com', // спам-ловушка
    'pavel@gmial.com',            // опечатка в домене: gmial → gmail
  ];

  const argument = process.argv[2];
  const emails = argument
    ? argument.split(',').map((e) => e.trim()).filter((e) => e.length > 0)
    : defaultList;

  console.log(`Чистка списка рассылки. Адресов на входе: ${emails.length}\n`);

  const summary = await filterMailingList(emails);

  console.log(`${pad('АДРЕС', 32)}${pad('СТАТУС', 11)}${pad('РЕШЕНИЕ', 13)}КОММЕНТАРИЙ`);
  for (const d of summary.decisions) {
    console.log(`${pad(d.email, 32)}${pad(d.statusRaw, 11)}${pad(d.action, 13)}${d.reason}`);
  }

  if (summary.typos.length > 0) {
    console.log('\nПОДСКАЗКИ ПО ОПЕЧАТКАМ (это спасённый контакт, а не потерянный):');
    for (const d of summary.typos) {
      console.log(`  [~] возможно, опечатка: ${d.email} → ${d.didYouMean}`);
    }
  }

  if (summary.spamtraps.length > 0) {
    console.log(`\n!!! СПАМ-ЛОВУШКА В СПИСКЕ: ${summary.spamtraps.length} !!!`);
    for (const d of summary.spamtraps) {
      console.log(`  ${d.email}`);
    }
    console.log('  Такой адрес никто не заводил и ни на что не подписывал — его публикуют,');
    console.log('  чтобы ловить рассылки по купленным базам. Одно попадание роняет репутацию');
    console.log('  домена-отправителя, после чего в спам уходит ВСЯ рассылка, включая письма');
    console.log('  живым клиентам. Удалять безусловно и выяснять, откуда адрес взялся.');
  }

  const count = (action: Action): number => summary.decisions.filter((d) => d.action === action).length;

  console.log('\nИТОГО');
  console.log(`  Проверено адресов:      ${summary.decisions.length}`);
  console.log(`  К отправке (SEND):      ${count('SEND')}`);
  console.log(`  На свой риск (RISKY):   ${count('SEND_RISKY')}`);
  console.log(`  Повторить позже:        ${count('RETRY_LATER')}  (не тарифицируется)`);
  console.log(`  Выброшено (DROP):       ${count('DROP')}`);
  console.log(
    `  Bounce rate без чистки: ${summary.bounceRate.toFixed(1)} %  ` +
      `(несуществующих ящиков: ${summary.invalid} из ${summary.decisions.length})`,
  );
  if (summary.spamtraps.length > 0) {
    console.log(`  Спам-ловушек:           ${summary.spamtraps.length}  <-- критично`);
  }
}

// Запуск только когда файл выполняется напрямую, а не импортируется.
if (process.argv[1]?.includes('index')) {
  main().catch((error: unknown) => {
    console.error('Ошибка:', error instanceof Error ? error.message : error);
    process.exit(1);
  });
}

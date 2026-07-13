<?php

/**
 * Клиент API «Проверка почты» Atlorium — существует ли ящик, не спам-ловушка ли это.
 *
 * Запуск (работает сразу, без регистрации — на демо-ключе):
 *   php main.php
 *   php main.php "anna@example.com,promo@spamtrap.example.com"
 *
 * Боевой ключ: получить на https://atlorium.com и положить в переменную окружения
 * ATLORIUM_API_KEY. Код при этом не меняется.
 */

declare(strict_types=1);

/**
 * Публичный демо-ключ. С ним API отвечает правдоподобными МОКАМИ (не реальными
 * данными) — чтобы можно было встроить и протестировать интеграцию до оплаты.
 * Ответы детерминированы: один и тот же адрес всегда даёт один и тот же результат,
 * поэтому на них можно писать стабильные тесты.
 */
const SANDBOX_KEY = 'ak_sandbox_demo_mockdata_v1';

// Проверка ящика — синхронное обращение к почтовому серверу домена, он может тянуть
// с ответом. Отсюда таймаут заметно больше, чем у справочников.
const TIMEOUT = 30;

// Проверка почты — платный сервис, лимит жёсткий.
// Список рассылки длиннее двух адресов гарантированно упрётся в 429 — поэтому
// повтор после паузы здесь не «на всякий случай», а штатный режим работы.
const RETRY_DELAY = 30;
const MAX_RETRIES = 3;

// Потолок ожидания. Исчерпав ЧАСОВОЙ лимит, сервер честно отвечает Retry-After на
// 40+ минут — и клиент, слепо доверяющий заголовку, «зависнет» на эти 40 минут
// (а в CI просто съест весь бюджет джоба). Дольше потолка не ждём.
const MAX_RETRY_DELAY = 120;

// Решения по адресу.
const SEND = 'SEND';               // отправляем
const SEND_RISKY = 'SEND_RISKY';   // можно отправить, но на свой риск
const RETRY_LATER = 'RETRY_LATER'; // проверить не удалось, повторить позже
const DROP = 'DROP';               // из списка выбросить

/** Ошибка API: HTTP-код разложен в человекочитаемую причину. */
final class AtloriumError extends RuntimeException
{
    private const REASONS = [
        400 => 'Адрес не передан или синтаксически некорректен (запрос НЕ тарифицируется)',
        401 => 'API-ключ отсутствует, просрочен или недействителен',
        402 => 'Недостаточно кредитов на балансе — пополните на https://atlorium.com',
        429 => 'Превышен лимит запросов — повторите позже',
        503 => 'Сервис проверки почты временно недоступен (за сбой на своей стороне мы не списываем деньги)',
    ];

    public function __construct(public readonly int $status, string $body)
    {
        $reason = self::REASONS[$status] ?? 'Неизвестная ошибка';
        parent::__construct(sprintf(
            'HTTP %d: %s. Ответ сервера: %s',
            $status,
            $reason,
            mb_substr($body, 0, 200)
        ));
    }
}

final class EmailValidationClient
{
    private string $apiKey;
    private string $baseUrl;

    public function __construct(?string $apiKey = null, ?string $baseUrl = null)
    {
        $this->apiKey = $apiKey ?? (getenv('ATLORIUM_API_KEY') ?: SANDBOX_KEY);
        $this->baseUrl = $baseUrl ?? (getenv('ATLORIUM_BASE_URL') ?: 'https://atlorium.com');
    }

    public function isSandbox(): bool
    {
        return $this->apiKey === SANDBOX_KEY;
    }

    /**
     * Проверка одного адреса: GET /api/EmailValidation?email=...
     *
     * Письмо на адрес НЕ отправляется: провайдер обращается к почтовому серверу домена
     * и выясняет, принял бы тот письмо на этот ящик.
     *
     * @return array<string, mixed>
     */
    public function verifyEmail(string $email): array
    {
        $url = $this->baseUrl . '/api/EmailValidation?' . http_build_query(['email' => $email]);

        for ($attempt = 0; ; $attempt++) {
            $curl = curl_init($url);
            curl_setopt_array($curl, [
                CURLOPT_RETURNTRANSFER => true,
                CURLOPT_TIMEOUT => TIMEOUT,
                CURLOPT_HEADER => true,
                CURLOPT_HTTPHEADER => [
                    'Authorization: Bearer ' . $this->apiKey,
                    'Accept: application/json',
                ],
            ]);

            $raw = curl_exec($curl);
            if ($raw === false) {
                $error = curl_error($curl);
                curl_close($curl);
                throw new RuntimeException("Сетевая ошибка: {$error}");
            }

            $status = curl_getinfo($curl, CURLINFO_RESPONSE_CODE);
            $headerSize = curl_getinfo($curl, CURLINFO_HEADER_SIZE);
            curl_close($curl);

            $headers = substr((string) $raw, 0, $headerSize);
            $body = substr((string) $raw, $headerSize);

            // 429 — не поломка, а реальный лимит продукта. Ждём и повторяем.
            if ($status === 429 && $attempt < MAX_RETRIES) {
                $delay = self::retryAfter($headers);
                if ($delay === 0) {
                    // Сервер просит ждать дольше потолка — значит, исчерпан часовой лимит.
                    // Спать 40 минут бессмысленно: честно говорим об этом и выходим.
                    throw new AtloriumError(429, 'лимит запросов по IP исчерпан, повторите позже');
                }
                fwrite(STDERR, "  ... лимит запросов, пауза {$delay} с\n");
                sleep($delay);
                continue;
            }

            if ($status !== 200) {
                throw new AtloriumError($status, $body);
            }

            return json_decode($body, true, 512, JSON_THROW_ON_ERROR);
        }
    }

    /**
     * Сколько ждать после 429. Ноль/мусор и слишком большие значения не берём на веру.
     *
     * Значение 0 (или мусор) означало бы «повторяй немедленно» — клиент ушёл бы в
     * busy-loop и выжег остаток лимита за секунду. Значение в 40+ минут (так сервер
     * отвечает на исчерпанный часовой лимит) означало бы «спи почти час» — этого мы
     * тоже не делаем. Возвращаем 0, если ждать бессмысленно долго: вызывающий сдастся.
     */
    private static function retryAfter(string $headers): int
    {
        if (preg_match('/^Retry-After:\s*(\d+)/mi', $headers, $match) !== 1) {
            return RETRY_DELAY;
        }

        $seconds = (int) $match[1];
        if ($seconds <= 0) {
            return RETRY_DELAY;
        }

        return $seconds <= MAX_RETRY_DELAY ? $seconds : 0;
    }
}

// ── Применение данных: чистка списка рассылки ─────────────────────────────────
// Отчёт по адресу сам по себе — просто JSON. Ценность появляется, когда по нему
// принимают решение: слать письмо или нет. Ниже — ровно это решение.

/**
 * Что делать с адресом, исходя из вердикта проверки.
 *
 * Ключ spamtrap вынесен отдельно: это не «ещё один DROP», а причина остановить
 * рассылку и разобраться, откуда взялся адрес.
 *
 * @param array<string, mixed> $report
 * @return array{email: string, statusRaw: string, action: string, reason: string, spamtrap: bool, didYouMean: ?string}
 */
function decide(array $report): array
{
    $status = $report['status'] ?? '';
    $sub = $report['subStatus'] ?? null;

    $spamtrap = false;

    switch ($status) {
        case 'Valid':
            $action = SEND;
            $reason = 'Ящик существует, домен принимает почту';
            break;

        case 'CatchAll':
            // Домен отвечает «принято» на ЛЮБОЙ адрес, поэтому существование конкретного
            // ящика проверить снаружи невозможно. Это не «плохо» — это неопределённость.
            // Слать можно, но отдельным сегментом и следя за bounce rate.
            $action = SEND_RISKY;
            $reason = 'Домен принимает всё подряд — доставляемость не гарантирована';
            break;

        case 'Unknown':
            // Сервер домена применил greylisting, не ответил или заблокировал проверку.
            // За такой исход деньги НЕ списываются — адрес просто проверяем позже.
            $action = RETRY_LATER;
            $reason = 'Сервер домена не ответил (greylisting) — запрос не тарифицирован';
            break;

        case 'Invalid':
            // Ящика не существует. Отправка = hard bounce, а bounce rate выше 2 %
            // почтовые провайдеры считают признаком грязной базы.
            $action = DROP;
            $reason = 'Ящика не существует — письмо отскочит (hard bounce)';
            break;

        case 'DoNotMail':
            // Технически ящик может работать, но рассылать на него нельзя.
            $action = DROP;
            $reason = match ($sub) {
                'role_based' => 'Ролевой адрес (info@, sales@) — читает не человек, а отдел',
                'disposable' => 'Одноразовый ящик — создан на 10 минут, писать бессмысленно',
                default => 'Адрес из списка «не писать»',
            };
            break;

        case 'Abuse':
            // Владелец адреса уже жаловался на спам. Ещё одна жалоба — минус к репутации.
            $action = DROP;
            $reason = 'Жалобщик: ранее отмечал письма как спам';
            break;

        case 'SpamTrap':
            // САМОЕ ВАЖНОЕ, ЧТО ДАЁТ СЕРВИС.
            // Спам-ловушка — адрес, который никто не заводил и на который никто не
            // подписывался: его специально публикуют, чтобы поймать тех, кто рассылает
            // по купленным и спарсенным базам. Ящик отвечает как живой, поэтому отличить
            // его самому невозможно. Одно попадание — и репутация домена-отправителя
            // падает, а в спам уходит ВСЯ рассылка, включая письма живым клиентам.
            $action = DROP;
            $reason = 'СПАМ-ЛОВУШКА — убивает репутацию домена-отправителя';
            $spamtrap = true;
            break;

        default:
            // Провайдер вернул статус, которого мы не знаем. Не рискуем.
            $action = DROP;
            $reason = 'Неизвестный статус: ' . ($report['statusRaw'] ?? '');
    }

    return [
        'email' => (string) ($report['email'] ?? ''),
        'statusRaw' => (string) ($report['statusRaw'] ?? ''),
        'action' => $action,
        'reason' => $reason,
        'spamtrap' => $spamtrap,
        'didYouMean' => $report['didYouMean'] ?? null,
    ];
}

/**
 * Чистка списка рассылки: каждый адрес проверяется и получает решение.
 *
 * @param list<string> $emails
 * @return list<array{email: string, statusRaw: string, action: string, reason: string, spamtrap: bool, didYouMean: ?string}>
 */
function filterMailingList(EmailValidationClient $client, array $emails): array
{
    $decisions = [];
    foreach ($emails as $email) {
        $decisions[] = decide($client->verifyEmail($email));
    }

    return $decisions;
}

/** Дополняет строку пробелами до нужной ширины, чтобы колонки не разъезжались. */
function pad(string $value, int $width): string
{
    $length = mb_strlen($value);

    return $length >= $width ? $value : $value . str_repeat(' ', $width - $length);
}

// ── Демонстрация ─────────────────────────────────────────────────────────────

$client = new EmailValidationClient();

if ($client->isSandbox()) {
    echo "Демо-ключ: ответы сгенерированы (моки), не реальные данные.\n\n";
}

// Список короткий намеренно: проверка почты — платный сервис с жёстким лимитом.
// Домены ниже — сценарии песочницы, см. README.
$emails = [
    'anna.petrova@example.com',   // обычный живой ящик
    'ivan@invalid.example.com',   // ящика не существует
    'promo@spamtrap.example.com', // спам-ловушка
    'pavel@gmial.com',            // опечатка в домене: gmial → gmail
];

if (isset($argv[1])) {
    $emails = array_values(array_filter(array_map('trim', explode(',', $argv[1])), fn (string $e): bool => $e !== ''));
}

echo 'Чистка списка рассылки. Адресов на входе: ' . count($emails) . "\n\n";

try {
    $decisions = filterMailingList($client, $emails);
} catch (AtloriumError $error) {
    fwrite(STDERR, "Ошибка: {$error->getMessage()}\n");
    exit(1);
}

echo pad('АДРЕС', 32) . pad('СТАТУС', 11) . pad('РЕШЕНИЕ', 13) . "КОММЕНТАРИЙ\n";
foreach ($decisions as $d) {
    echo pad($d['email'], 32) . pad($d['statusRaw'], 11) . pad($d['action'], 13) . $d['reason'] . "\n";
}

$typos = array_filter($decisions, fn (array $d): bool => $d['didYouMean'] !== null);
if ($typos !== []) {
    echo "\nПОДСКАЗКИ ПО ОПЕЧАТКАМ (это спасённый контакт, а не потерянный):\n";
    foreach ($typos as $d) {
        echo "  [~] возможно, опечатка: {$d['email']} → {$d['didYouMean']}\n";
    }
}

$spamtraps = array_filter($decisions, fn (array $d): bool => $d['spamtrap']);
if ($spamtraps !== []) {
    echo "\n!!! СПАМ-ЛОВУШКА В СПИСКЕ: " . count($spamtraps) . " !!!\n";
    foreach ($spamtraps as $d) {
        echo "  {$d['email']}\n";
    }
    echo "  Такой адрес никто не заводил и ни на что не подписывал — его публикуют,\n";
    echo "  чтобы ловить рассылки по купленным базам. Одно попадание роняет репутацию\n";
    echo "  домена-отправителя, после чего в спам уходит ВСЯ рассылка, включая письма\n";
    echo "  живым клиентам. Удалять безусловно и выяснять, откуда адрес взялся.\n";
}

$count = fn (string $action): int => count(array_filter($decisions, fn (array $d): bool => $d['action'] === $action));

// Какой был бы bounce rate, если бы список отправили как есть.
$invalid = count(array_filter($decisions, fn (array $d): bool => $d['statusRaw'] === 'invalid'));
$bounceRate = $decisions === [] ? 0.0 : 100.0 * $invalid / count($decisions);

echo "\nИТОГО\n";
echo '  Проверено адресов:      ' . count($decisions) . "\n";
echo '  К отправке (SEND):      ' . $count(SEND) . "\n";
echo '  На свой риск (RISKY):   ' . $count(SEND_RISKY) . "\n";
echo '  Повторить позже:        ' . $count(RETRY_LATER) . "  (не тарифицируется)\n";
echo '  Выброшено (DROP):       ' . $count(DROP) . "\n";
printf("  Bounce rate без чистки: %.1f %%  (несуществующих ящиков: %d из %d)\n",
    $bounceRate, $invalid, count($decisions));

if ($spamtraps !== []) {
    echo '  Спам-ловушек:           ' . count($spamtraps) . "  <-- критично\n";
}

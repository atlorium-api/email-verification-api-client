// Клиент API «Проверка почты» Atlorium — существует ли ящик, не спам-ловушка ли это.
//
// Запуск (работает сразу, без регистрации — на демо-ключе):
//     dotnet run
//     dotnet run "anna@example.com,promo@spamtrap.example.com"
//
// Боевой ключ: получить на https://atlorium.com и положить в переменную окружения
// ATLORIUM_API_KEY. Код при этом не меняется.

using System.Globalization;
using System.Net;
using System.Net.Http.Headers;
using System.Text.Json;

// Публичный демо-ключ. С ним API отвечает правдоподобными МОКАМИ (не реальными
// данными) — чтобы можно было встроить и протестировать интеграцию до оплаты.
// Ответы детерминированы: один и тот же адрес всегда даёт один и тот же результат,
// поэтому на них можно писать стабильные тесты.
const string SandboxKey = "ak_sandbox_demo_mockdata_v1";

var apiKey = Environment.GetEnvironmentVariable("ATLORIUM_API_KEY") ?? SandboxKey;
var baseUrl = Environment.GetEnvironmentVariable("ATLORIUM_BASE_URL") ?? "https://atlorium.com";

// Проверка ящика — синхронное обращение к почтовому серверу домена, он может тянуть
// с ответом. Отсюда таймаут заметно больше, чем у справочников.
using var http = new HttpClient
{
    BaseAddress = new Uri(baseUrl),
    Timeout = TimeSpan.FromSeconds(30),
};
http.DefaultRequestHeaders.Authorization = new AuthenticationHeaderValue("Bearer", apiKey);
http.DefaultRequestHeaders.Accept.Add(new MediaTypeWithQualityHeaderValue("application/json"));

var client = new EmailValidationClient(http);

if (apiKey == SandboxKey)
{
    Console.WriteLine("Демо-ключ: ответы сгенерированы (моки), не реальные данные.\n");
}

// Список короткий намеренно: проверка почты — платный сервис с жёстким лимитом.
// Домены ниже — сценарии песочницы, см. README.
string[] defaultList =
[
    "anna.petrova@example.com",   // обычный живой ящик
    "ivan@invalid.example.com",   // ящика не существует
    "promo@spamtrap.example.com", // спам-ловушка
    "pavel@gmial.com",            // опечатка в домене: gmial → gmail
];

var emails = args.Length > 0
    ? args[0].Split(',', StringSplitOptions.RemoveEmptyEntries | StringSplitOptions.TrimEntries)
    : defaultList;

Console.WriteLine($"Чистка списка рассылки. Адресов на входе: {emails.Length}\n");

List<Decision> decisions;
try
{
    decisions = await MailingList.FilterAsync(client, emails);
}
catch (AtloriumException error)
{
    Console.Error.WriteLine($"Ошибка: {error.Message}");
    return 1;
}

Console.WriteLine($"{Pad("АДРЕС", 32)}{Pad("СТАТУС", 11)}{Pad("РЕШЕНИЕ", 13)}КОММЕНТАРИЙ");
foreach (var d in decisions)
{
    Console.WriteLine($"{Pad(d.Email, 32)}{Pad(d.StatusRaw, 11)}{Pad(d.Action, 13)}{d.Reason}");
}

var typos = decisions.Where(d => d.DidYouMean is not null).ToList();
if (typos.Count > 0)
{
    Console.WriteLine("\nПОДСКАЗКИ ПО ОПЕЧАТКАМ (это спасённый контакт, а не потерянный):");
    foreach (var d in typos)
    {
        Console.WriteLine($"  [~] возможно, опечатка: {d.Email} → {d.DidYouMean}");
    }
}

var spamtraps = decisions.Where(d => d.SpamTrap).ToList();
if (spamtraps.Count > 0)
{
    Console.WriteLine($"\n!!! СПАМ-ЛОВУШКА В СПИСКЕ: {spamtraps.Count} !!!");
    foreach (var d in spamtraps)
    {
        Console.WriteLine($"  {d.Email}");
    }
    Console.WriteLine("  Такой адрес никто не заводил и ни на что не подписывал — его публикуют,");
    Console.WriteLine("  чтобы ловить рассылки по купленным базам. Одно попадание роняет репутацию");
    Console.WriteLine("  домена-отправителя, после чего в спам уходит ВСЯ рассылка, включая письма");
    Console.WriteLine("  живым клиентам. Удалять безусловно и выяснять, откуда адрес взялся.");
}

// Какой был бы bounce rate, если бы список отправили как есть.
var invalid = decisions.Count(d => d.StatusRaw == "invalid");
var bounceRate = decisions.Count > 0 ? 100.0 * invalid / decisions.Count : 0.0;

Console.WriteLine("\nИТОГО");
Console.WriteLine($"  Проверено адресов:      {decisions.Count}");
Console.WriteLine($"  К отправке (SEND):      {decisions.Count(d => d.Action == MailingList.Send)}");
Console.WriteLine($"  На свой риск (RISKY):   {decisions.Count(d => d.Action == MailingList.SendRisky)}");
Console.WriteLine($"  Повторить позже:        {decisions.Count(d => d.Action == MailingList.RetryLater)}  (не тарифицируется)");
Console.WriteLine($"  Выброшено (DROP):       {decisions.Count(d => d.Action == MailingList.Drop)}");

// InvariantCulture: иначе под локалью с запятой в качестве разделителя вывод
// разъедется с остальными пятью примерами (25,0 вместо 25.0).
Console.WriteLine($"  Bounce rate без чистки: {bounceRate.ToString("F1", CultureInfo.InvariantCulture)} %  "
                  + $"(несуществующих ящиков: {invalid} из {decisions.Count})");

if (spamtraps.Count > 0)
{
    Console.WriteLine($"  Спам-ловушек:           {spamtraps.Count}  <-- критично");
}

return 0;

// Дополняет строку пробелами до нужной ширины, чтобы колонки не разъезжались.
static string Pad(string value, int width) => value.PadRight(width);

// ── Клиент ───────────────────────────────────────────────────────────────────

/// <summary>Ошибка API: HTTP-код разложен в человекочитаемую причину.</summary>
public sealed class AtloriumException(HttpStatusCode status, string body)
    : Exception($"HTTP {(int)status}: {Explain(status)}. Ответ сервера: {body[..Math.Min(200, body.Length)]}")
{
    public HttpStatusCode Status { get; } = status;

    private static string Explain(HttpStatusCode status) => (int)status switch
    {
        400 => "Адрес не передан или синтаксически некорректен (запрос НЕ тарифицируется)",
        401 => "API-ключ отсутствует, просрочен или недействителен",
        402 => "Недостаточно кредитов на балансе — пополните на https://atlorium.com",
        429 => "Превышен лимит запросов — повторите позже",
        503 => "Сервис проверки почты временно недоступен (за сбой на своей стороне мы не списываем деньги)",
        _ => "Неизвестная ошибка",
    };
}

public sealed class EmailValidationClient(HttpClient http)
{
    private static readonly JsonSerializerOptions JsonOptions = new(JsonSerializerDefaults.Web);

    // Проверка почты — платный сервис, лимит жёсткий.
    // Список рассылки длиннее двух адресов гарантированно упрётся в 429 — поэтому
    // повтор после паузы здесь не «на всякий случай», а штатный режим работы.
    private static readonly TimeSpan RetryDelay = TimeSpan.FromSeconds(30);
    private const int MaxRetries = 3;

    // Потолок ожидания. Исчерпав ЧАСОВОЙ лимит, сервер честно отвечает Retry-After на
    // 40+ минут — и клиент, слепо доверяющий заголовку, «зависнет» на эти 40 минут
    // (а в CI просто съест весь бюджет джоба). Дольше потолка не ждём.
    private static readonly TimeSpan MaxRetryDelay = TimeSpan.FromSeconds(120);

    /// <summary>
    /// Проверка одного адреса: GET /api/EmailValidation?email=...
    ///
    /// Письмо на адрес НЕ отправляется: провайдер обращается к почтовому серверу домена
    /// и выясняет, принял бы тот письмо на этот ящик.
    /// </summary>
    public async Task<EmailReport> VerifyAsync(string email)
    {
        var path = $"/api/EmailValidation?email={Uri.EscapeDataString(email)}";

        for (var attempt = 0; ; attempt++)
        {
            using var response = await http.GetAsync(path);

            // 429 — не поломка, а реальный лимит продукта. Ждём и повторяем.
            if (response.StatusCode == HttpStatusCode.TooManyRequests && attempt < MaxRetries)
            {
                var delay = RetryAfter(response);
                if (delay == TimeSpan.Zero)
                {
                    // Сервер просит ждать дольше потолка — значит, исчерпан часовой лимит.
                    // Спать 40 минут бессмысленно: честно говорим об этом и выходим.
                    throw new AtloriumException(HttpStatusCode.TooManyRequests,
                        "лимит запросов по IP исчерпан, повторите позже");
                }
                Console.Error.WriteLine($"  ... лимит запросов, пауза {delay.TotalSeconds:F0} с");
                await Task.Delay(delay);
                continue;
            }

            var body = await response.Content.ReadAsStringAsync();
            if (!response.IsSuccessStatusCode)
            {
                throw new AtloriumException(response.StatusCode, body);
            }

            return JsonSerializer.Deserialize<EmailReport>(body, JsonOptions)
                   ?? throw new InvalidOperationException("Пустой ответ API.");
        }
    }

    /// <summary>
    /// Сколько ждать после 429. Ноль/мусор и слишком большие значения не берём на веру.
    ///
    /// Значение 0 (или мусор) означало бы «повторяй немедленно» — клиент ушёл бы в
    /// busy-loop и выжег остаток лимита за секунду. Значение в 40+ минут (так сервер
    /// отвечает на исчерпанный часовой лимит) означало бы «спи почти час» — этого мы
    /// тоже не делаем. Возвращаем TimeSpan.Zero, если ждать бессмысленно долго.
    /// </summary>
    private static TimeSpan RetryAfter(HttpResponseMessage response)
    {
        var seconds = response.Headers.RetryAfter?.Delta?.TotalSeconds ?? 0;
        if (seconds <= 0)
        {
            return RetryDelay;
        }
        var delay = TimeSpan.FromSeconds(seconds);
        return delay <= MaxRetryDelay ? delay : TimeSpan.Zero;
    }
}

// ── Модель ответа ────────────────────────────────────────────────────────────

/// <summary>Карточка проверки одного адреса.</summary>
public sealed record EmailReport
{
    public string Email { get; init; } = "";

    /// <summary>Valid | Invalid | CatchAll | Unknown | SpamTrap | Abuse | DoNotMail.</summary>
    public string Status { get; init; } = "";

    /// <summary>Статус как его вернул источник: "valid", "catch-all", "do_not_mail".</summary>
    public string StatusRaw { get; init; } = "";

    /// <summary>Уточнение: "mailbox_not_found", "disposable", "role_based", "greylisted".</summary>
    public string? SubStatus { get; init; }

    public string? Account { get; init; }
    public string? Domain { get; init; }
    public bool FreeEmail { get; init; }
    public bool? CatchAllDomain { get; init; }

    /// <summary>Подсказка по опечатке в домене: gmial.com → gmail.com.</summary>
    public string? DidYouMean { get; init; }

    public int? DomainAgeDays { get; init; }
    public string? SmtpProvider { get; init; }
    public bool MxFound { get; init; }
    public string? MxRecord { get; init; }
    public string? ActiveInDays { get; init; }
    public string? ActiveFirstSeen { get; init; }
    public DateTimeOffset? ProcessedAtUtc { get; init; }
    public long ElapsedMs { get; init; }

    /// <summary>true только при Status = Valid. catch-all и unknown — неопределённость.</summary>
    public bool Deliverable { get; init; }
}

// ── Применение данных: чистка списка рассылки ─────────────────────────────────
// Отчёт по адресу сам по себе — просто JSON. Ценность появляется, когда по нему
// принимают решение: слать письмо или нет. Ниже — ровно это решение.

/// <summary>
/// Решение по адресу. SpamTrap вынесен в отдельный флаг: это не «ещё один DROP»,
/// а причина остановить рассылку и разобраться, откуда взялся адрес.
/// </summary>
public sealed record Decision(
    string Email,
    string StatusRaw,
    string Action,
    string Reason,
    bool SpamTrap = false,
    string? DidYouMean = null);

public static class MailingList
{
    public const string Send = "SEND";              // отправляем
    public const string SendRisky = "SEND_RISKY";   // можно отправить, но на свой риск
    public const string RetryLater = "RETRY_LATER"; // проверить не удалось, повторить позже
    public const string Drop = "DROP";              // из списка выбросить

    /// <summary>Чистка списка рассылки: каждый адрес проверяется и получает решение.</summary>
    public static async Task<List<Decision>> FilterAsync(EmailValidationClient client, IEnumerable<string> emails)
    {
        var decisions = new List<Decision>();
        foreach (var email in emails)
        {
            decisions.Add(Decide(await client.VerifyAsync(email)));
        }
        return decisions;
    }

    /// <summary>Что делать с адресом, исходя из вердикта проверки.</summary>
    public static Decision Decide(EmailReport report) => report.Status switch
    {
        "Valid" => new Decision(report.Email, report.StatusRaw, Send,
            "Ящик существует, домен принимает почту", DidYouMean: report.DidYouMean),

        // Домен отвечает «принято» на ЛЮБОЙ адрес, поэтому существование конкретного
        // ящика проверить снаружи невозможно. Это не «плохо» — это неопределённость.
        // Слать можно, но отдельным сегментом и следя за bounce rate.
        "CatchAll" => new Decision(report.Email, report.StatusRaw, SendRisky,
            "Домен принимает всё подряд — доставляемость не гарантирована", DidYouMean: report.DidYouMean),

        // Сервер домена применил greylisting, не ответил или заблокировал проверку.
        // За такой исход деньги НЕ списываются — адрес просто проверяем позже.
        "Unknown" => new Decision(report.Email, report.StatusRaw, RetryLater,
            "Сервер домена не ответил (greylisting) — запрос не тарифицирован", DidYouMean: report.DidYouMean),

        // Ящика не существует. Отправка = hard bounce, а bounce rate выше 2 %
        // почтовые провайдеры считают признаком грязной базы.
        "Invalid" => new Decision(report.Email, report.StatusRaw, Drop,
            "Ящика не существует — письмо отскочит (hard bounce)", DidYouMean: report.DidYouMean),

        // Технически ящик может работать, но рассылать на него нельзя.
        "DoNotMail" => new Decision(report.Email, report.StatusRaw, Drop, report.SubStatus switch
        {
            "role_based" => "Ролевой адрес (info@, sales@) — читает не человек, а отдел",
            "disposable" => "Одноразовый ящик — создан на 10 минут, писать бессмысленно",
            _ => "Адрес из списка «не писать»",
        }, DidYouMean: report.DidYouMean),

        // Владелец адреса уже жаловался на спам. Ещё одна жалоба — минус к репутации.
        "Abuse" => new Decision(report.Email, report.StatusRaw, Drop,
            "Жалобщик: ранее отмечал письма как спам", DidYouMean: report.DidYouMean),

        // САМОЕ ВАЖНОЕ, ЧТО ДАЁТ СЕРВИС.
        // Спам-ловушка — адрес, который никто не заводил и на который никто не
        // подписывался: его специально публикуют, чтобы поймать тех, кто рассылает
        // по купленным и спарсенным базам. Ящик отвечает как живой, поэтому отличить
        // его самому невозможно. Одно попадание — и репутация домена-отправителя
        // падает, а в спам уходит ВСЯ рассылка, включая письма живым клиентам.
        "SpamTrap" => new Decision(report.Email, report.StatusRaw, Drop,
            "СПАМ-ЛОВУШКА — убивает репутацию домена-отправителя",
            SpamTrap: true, DidYouMean: report.DidYouMean),

        // Провайдер вернул статус, которого мы не знаем. Не рискуем.
        _ => new Decision(report.Email, report.StatusRaw, Drop,
            $"Неизвестный статус: {report.StatusRaw}", DidYouMean: report.DidYouMean),
    };
}

/*
 * Клиент API «Проверка почты» Atlorium — существует ли ящик, не спам-ловушка ли это.
 *
 * Запуск (работает сразу, без регистрации — на демо-ключе).
 * Начиная с Java 11 файл запускается напрямую, без компиляции и без зависимостей:
 *
 *     java Main.java
 *     java Main.java "anna@example.com,promo@spamtrap.example.com"
 *
 * Боевой ключ: получить на https://atlorium.com и положить в переменную окружения
 * ATLORIUM_API_KEY. Код при этом не меняется.
 */

import java.io.IOException;
import java.net.URI;
import java.net.URLEncoder;
import java.net.http.HttpClient;
import java.net.http.HttpRequest;
import java.net.http.HttpResponse;
import java.nio.charset.StandardCharsets;
import java.time.Duration;
import java.util.ArrayList;
import java.util.Arrays;
import java.util.List;
import java.util.Locale;
import java.util.Map;
import java.util.regex.Matcher;
import java.util.regex.Pattern;

public class Main {

    /**
     * Публичный демо-ключ. С ним API отвечает правдоподобными МОКАМИ (не реальными
     * данными) — чтобы можно было встроить и протестировать интеграцию до оплаты.
     * Ответы детерминированы: один и тот же адрес всегда даёт один и тот же результат,
     * поэтому на них можно писать стабильные тесты.
     */
    static final String SANDBOX_KEY = "ak_sandbox_demo_mockdata_v1";

    static final String API_KEY = envOr("ATLORIUM_API_KEY", SANDBOX_KEY);
    static final String BASE_URL = envOr("ATLORIUM_BASE_URL", "https://atlorium.com");

    /**
     * Проверка почты — платный сервис, лимит жёсткий.
     * Список рассылки длиннее двух адресов гарантированно упрётся в 429 — поэтому
     * повтор после паузы здесь не «на всякий случай», а штатный режим работы.
     */
    static final int RETRY_DELAY_SECONDS = 30;
    static final int MAX_RETRIES = 3;

    /**
     * Потолок ожидания. Исчерпав ЧАСОВОЙ лимит, сервер честно отвечает Retry-After на
     * 40+ минут — и клиент, слепо доверяющий заголовку, «зависнет» на эти 40 минут
     * (а в CI просто съест весь бюджет джоба). Дольше потолка не ждём.
     */
    static final int MAX_RETRY_DELAY_SECONDS = 120;

    // Проверка ящика — синхронное обращение к почтовому серверу домена, он может тянуть
    // с ответом. Отсюда таймаут заметно больше, чем у справочников.
    static final HttpClient CLIENT = HttpClient.newBuilder()
            .connectTimeout(Duration.ofSeconds(30))
            .build();

    static String envOr(String key, String fallback) {
        String value = System.getenv(key);
        return (value == null || value.isBlank()) ? fallback : value;
    }

    /** Ошибка API: HTTP-код разложен в человекочитаемую причину. */
    static class AtloriumException extends RuntimeException {
        private static final Map<Integer, String> REASONS = Map.of(
                400, "Адрес не передан или синтаксически некорректен (запрос НЕ тарифицируется)",
                401, "API-ключ отсутствует, просрочен или недействителен",
                402, "Недостаточно кредитов на балансе — пополните на https://atlorium.com",
                429, "Превышен лимит запросов — повторите позже",
                503, "Сервис проверки почты временно недоступен "
                        + "(за сбой на своей стороне мы не списываем деньги)");

        final int status;

        AtloriumException(int status, String body) {
            super("HTTP " + status + ": "
                    + REASONS.getOrDefault(status, "Неизвестная ошибка")
                    + ". Ответ сервера: " + body.substring(0, Math.min(200, body.length())));
            this.status = status;
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
    static int retryAfterSeconds(HttpResponse<String> response) {
        int seconds;
        try {
            seconds = Integer.parseInt(response.headers().firstValue("Retry-After").orElse(""));
        } catch (NumberFormatException ignored) {
            return RETRY_DELAY_SECONDS;
        }
        if (seconds <= 0) {
            return RETRY_DELAY_SECONDS;
        }
        return seconds <= MAX_RETRY_DELAY_SECONDS ? seconds : 0;
    }

    /**
     * Проверка одного адреса: GET /api/EmailValidation?email=...
     *
     * Письмо на адрес НЕ отправляется: провайдер обращается к почтовому серверу домена
     * и выясняет, принял бы тот письмо на этот ящик.
     */
    static String verifyEmail(String email) throws IOException, InterruptedException {
        String url = BASE_URL + "/api/EmailValidation?email="
                + URLEncoder.encode(email, StandardCharsets.UTF_8);

        HttpRequest request = HttpRequest.newBuilder(URI.create(url))
                .header("Authorization", "Bearer " + API_KEY)
                .header("Accept", "application/json")
                .timeout(Duration.ofSeconds(30))
                .GET()
                .build();

        for (int attempt = 0; ; attempt++) {
            HttpResponse<String> response = CLIENT.send(request, HttpResponse.BodyHandlers.ofString(StandardCharsets.UTF_8));

            // 429 — не поломка, а реальный лимит продукта. Ждём и повторяем.
            if (response.statusCode() == 429 && attempt < MAX_RETRIES) {
                int delay = retryAfterSeconds(response);
                if (delay == 0) {
                    // Сервер просит ждать дольше потолка — значит, исчерпан часовой лимит.
                    // Спать 40 минут бессмысленно: честно говорим об этом и выходим.
                    throw new AtloriumException(429, "лимит запросов по IP исчерпан, повторите позже");
                }
                System.err.println("  ... лимит запросов, пауза " + delay + " с");
                Thread.sleep(delay * 1000L);
                continue;
            }

            if (response.statusCode() != 200) {
                throw new AtloriumException(response.statusCode(), response.body());
            }
            return response.body();
        }
    }

    // ── Разбор JSON ──────────────────────────────────────────────────────────
    // Пример намеренно оставлен без внешних зависимостей, чтобы запускаться одной
    // командой `java Main.java`. В рабочем проекте берите Jackson или Gson и
    // маппьте ответ в полноценную запись — эти регулярки существуют только ради
    // отсутствия pom.xml.

    /** Строковое поле; null и для JSON-null, и для отсутствующего поля. */
    static String str(String json, String field) {
        Matcher matcher = Pattern.compile("\"" + field + "\"\\s*:\\s*\"((?:[^\"\\\\]|\\\\.)*)\"").matcher(json);
        return matcher.find() ? matcher.group(1).replace("\\\"", "\"") : null;
    }

    // ── Применение данных: чистка списка рассылки ─────────────────────────────
    // Отчёт по адресу сам по себе — просто JSON. Ценность появляется, когда по нему
    // принимают решение: слать письмо или нет. Ниже — ровно это решение.

    static final String SEND = "SEND";               // отправляем
    static final String SEND_RISKY = "SEND_RISKY";   // можно отправить, но на свой риск
    static final String RETRY_LATER = "RETRY_LATER"; // проверить не удалось, повторить позже
    static final String DROP = "DROP";               // из списка выбросить

    /**
     * Решение по адресу. spamTrap вынесен в отдельный флаг: это не «ещё один DROP»,
     * а причина остановить рассылку и разобраться, откуда взялся адрес.
     * didYouMean — подсказка по опечатке в домене (gmial.com → gmail.com).
     */
    record Decision(String email, String statusRaw, String action, String reason,
                    boolean spamTrap, String didYouMean) {
    }

    /** Что делать с адресом, исходя из вердикта проверки. */
    static Decision decide(String report) {
        String email = str(report, "email");
        String status = str(report, "status");
        String statusRaw = str(report, "statusRaw");
        String subStatus = str(report, "subStatus");
        String didYouMean = str(report, "didYouMean");

        String action;
        String reason;
        boolean spamTrap = false;

        switch (status == null ? "" : status) {
            case "Valid" -> {
                action = SEND;
                reason = "Ящик существует, домен принимает почту";
            }
            case "CatchAll" -> {
                // Домен отвечает «принято» на ЛЮБОЙ адрес, поэтому существование конкретного
                // ящика проверить снаружи невозможно. Это не «плохо» — это неопределённость.
                // Слать можно, но отдельным сегментом и следя за bounce rate.
                action = SEND_RISKY;
                reason = "Домен принимает всё подряд — доставляемость не гарантирована";
            }
            case "Unknown" -> {
                // Сервер домена применил greylisting, не ответил или заблокировал проверку.
                // За такой исход деньги НЕ списываются — адрес просто проверяем позже.
                action = RETRY_LATER;
                reason = "Сервер домена не ответил (greylisting) — запрос не тарифицирован";
            }
            case "Invalid" -> {
                // Ящика не существует. Отправка = hard bounce, а bounce rate выше 2 %
                // почтовые провайдеры считают признаком грязной базы.
                action = DROP;
                reason = "Ящика не существует — письмо отскочит (hard bounce)";
            }
            case "DoNotMail" -> {
                // Технически ящик может работать, но рассылать на него нельзя.
                action = DROP;
                if ("role_based".equals(subStatus)) {
                    reason = "Ролевой адрес (info@, sales@) — читает не человек, а отдел";
                } else if ("disposable".equals(subStatus)) {
                    reason = "Одноразовый ящик — создан на 10 минут, писать бессмысленно";
                } else {
                    reason = "Адрес из списка «не писать»";
                }
            }
            case "Abuse" -> {
                // Владелец адреса уже жаловался на спам. Ещё одна жалоба — минус к репутации.
                action = DROP;
                reason = "Жалобщик: ранее отмечал письма как спам";
            }
            case "SpamTrap" -> {
                // САМОЕ ВАЖНОЕ, ЧТО ДАЁТ СЕРВИС.
                // Спам-ловушка — адрес, который никто не заводил и на который никто не
                // подписывался: его специально публикуют, чтобы поймать тех, кто рассылает
                // по купленным и спарсенным базам. Ящик отвечает как живой, поэтому отличить
                // его самому невозможно. Одно попадание — и репутация домена-отправителя
                // падает, а в спам уходит ВСЯ рассылка, включая письма живым клиентам.
                action = DROP;
                reason = "СПАМ-ЛОВУШКА — убивает репутацию домена-отправителя";
                spamTrap = true;
            }
            default -> {
                // Провайдер вернул статус, которого мы не знаем. Не рискуем.
                action = DROP;
                reason = "Неизвестный статус: " + statusRaw;
            }
        }

        return new Decision(email, statusRaw, action, reason, spamTrap, didYouMean);
    }

    /** Чистка списка рассылки: каждый адрес проверяется и получает решение. */
    static List<Decision> filterMailingList(List<String> emails) throws IOException, InterruptedException {
        List<Decision> decisions = new ArrayList<>();
        for (String email : emails) {
            decisions.add(decide(verifyEmail(email)));
        }
        return decisions;
    }

    static long count(List<Decision> decisions, String action) {
        return decisions.stream().filter(d -> d.action().equals(action)).count();
    }

    /** Дополняет строку пробелами до нужной ширины, чтобы колонки не разъезжались. */
    static String pad(String value, int width) {
        return value.length() >= width ? value : value + " ".repeat(width - value.length());
    }

    public static void main(String[] args) throws Exception {
        if (API_KEY.equals(SANDBOX_KEY)) {
            System.out.println("Демо-ключ: ответы сгенерированы (моки), не реальные данные.\n");
        }

        // Список короткий намеренно: проверка почты — платный сервис с жёстким лимитом.
        // Домены ниже — сценарии песочницы, см. README.
        List<String> emails = List.of(
                "anna.petrova@example.com",   // обычный живой ящик
                "ivan@invalid.example.com",   // ящика не существует
                "promo@spamtrap.example.com", // спам-ловушка
                "pavel@gmial.com");           // опечатка в домене: gmial → gmail

        if (args.length > 0) {
            emails = Arrays.stream(args[0].split(","))
                    .map(String::trim)
                    .filter(e -> !e.isEmpty())
                    .toList();
        }

        System.out.println("Чистка списка рассылки. Адресов на входе: " + emails.size() + "\n");

        List<Decision> decisions;
        try {
            decisions = filterMailingList(emails);
        } catch (AtloriumException error) {
            System.err.println("Ошибка: " + error.getMessage());
            System.exit(1);
            return;
        }

        System.out.println(pad("АДРЕС", 32) + pad("СТАТУС", 11) + pad("РЕШЕНИЕ", 13) + "КОММЕНТАРИЙ");
        for (Decision d : decisions) {
            System.out.println(pad(d.email(), 32) + pad(d.statusRaw(), 11) + pad(d.action(), 13) + d.reason());
        }

        List<Decision> typos = decisions.stream().filter(d -> d.didYouMean() != null).toList();
        if (!typos.isEmpty()) {
            System.out.println("\nПОДСКАЗКИ ПО ОПЕЧАТКАМ (это спасённый контакт, а не потерянный):");
            for (Decision d : typos) {
                System.out.println("  [~] возможно, опечатка: " + d.email() + " → " + d.didYouMean());
            }
        }

        List<Decision> spamtraps = decisions.stream().filter(Decision::spamTrap).toList();
        if (!spamtraps.isEmpty()) {
            System.out.println("\n!!! СПАМ-ЛОВУШКА В СПИСКЕ: " + spamtraps.size() + " !!!");
            for (Decision d : spamtraps) {
                System.out.println("  " + d.email());
            }
            System.out.println("  Такой адрес никто не заводил и ни на что не подписывал — его публикуют,");
            System.out.println("  чтобы ловить рассылки по купленным базам. Одно попадание роняет репутацию");
            System.out.println("  домена-отправителя, после чего в спам уходит ВСЯ рассылка, включая письма");
            System.out.println("  живым клиентам. Удалять безусловно и выяснять, откуда адрес взялся.");
        }

        // Какой был бы bounce rate, если бы список отправили как есть.
        long invalid = decisions.stream().filter(d -> "invalid".equals(d.statusRaw())).count();
        double bounceRate = decisions.isEmpty() ? 0 : 100.0 * invalid / decisions.size();

        System.out.println("\nИТОГО");
        System.out.println("  Проверено адресов:      " + decisions.size());
        System.out.println("  К отправке (SEND):      " + count(decisions, SEND));
        System.out.println("  На свой риск (RISKY):   " + count(decisions, SEND_RISKY));
        System.out.println("  Повторить позже:        " + count(decisions, RETRY_LATER) + "  (не тарифицируется)");
        System.out.println("  Выброшено (DROP):       " + count(decisions, DROP));
        // Locale.ROOT: иначе под локалью с запятой в качестве разделителя вывод
        // разъедется с остальными пятью примерами (25,0 вместо 25.0).
        System.out.printf(Locale.ROOT, "  Bounce rate без чистки: %.1f %%  (несуществующих ящиков: %d из %d)%n",
                bounceRate, invalid, decisions.size());
        if (!spamtraps.isEmpty()) {
            System.out.println("  Спам-ловушек:           " + spamtraps.size() + "  <-- критично");
        }
    }
}

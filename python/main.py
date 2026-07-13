"""
Клиент API «Проверка почты» Atlorium — существует ли ящик, не спам-ловушка ли это.

Запуск (работает сразу, без регистрации — на демо-ключе):
    pip install -r requirements.txt
    python main.py
    python main.py "anna@example.com,promo@spamtrap.example.com"

Боевой ключ: получить на https://atlorium.com и положить в переменную окружения
ATLORIUM_API_KEY. Код при этом не меняется.
"""

import os
import sys
import time
from dataclasses import dataclass, field

import requests

# Публичный демо-ключ. С ним API отвечает правдоподобными МОКАМИ (не реальными
# данными) — чтобы можно было встроить и протестировать интеграцию до оплаты.
# Ответы детерминированы: один и тот же адрес всегда даёт один и тот же результат,
# поэтому на них можно писать стабильные тесты.
SANDBOX_KEY = "ak_sandbox_demo_mockdata_v1"

API_KEY = os.environ.get("ATLORIUM_API_KEY", SANDBOX_KEY)
BASE_URL = os.environ.get("ATLORIUM_BASE_URL", "https://atlorium.com")

# Проверка ящика — синхронное обращение к почтовому серверу домена, он может тянуть
# с ответом. Отсюда таймаут заметно больше, чем у справочников.
TIMEOUT = 30

# Проверка почты — платный сервис, лимит жёсткий.
# Список рассылки длиннее двух адресов гарантированно упрётся в 429 — поэтому
# повтор после паузы здесь не «на всякий случай», а штатный режим работы.
RETRY_DELAY = 30
MAX_RETRIES = 3

# Потолок ожидания. Исчерпав ЧАСОВОЙ лимит, сервер честно отвечает Retry-After на
# 40+ минут — и клиент, слепо доверяющий заголовку, «зависнет» на эти 40 минут
# (а в CI просто съест весь бюджет джоба). Дольше потолка не ждём: сообщаем, что
# квота исчерпана, и выходим.
MAX_RETRY_DELAY = 120


class AtloriumError(RuntimeError):
    """Ошибка API. Код HTTP разложен в человекочитаемую причину."""

    REASONS = {
        400: "Адрес не передан или синтаксически некорректен (запрос НЕ тарифицируется)",
        401: "API-ключ отсутствует, просрочен или недействителен",
        402: "Недостаточно кредитов на балансе — пополните на https://atlorium.com",
        429: "Превышен лимит запросов — повторите позже",
        503: "Сервис проверки почты временно недоступен "
             "(за сбой на своей стороне мы не списываем деньги)",
    }

    def __init__(self, status: int, body: str):
        reason = self.REASONS.get(status, "Неизвестная ошибка")
        super().__init__(f"HTTP {status}: {reason}. Ответ сервера: {body[:200]}")
        self.status = status


def retry_after(response: requests.Response) -> int:
    """Сколько ждать после 429. Ноль/мусор и слишком большие значения не берём на веру.

    Значение 0 (или мусор) означало бы «повторяй немедленно» — клиент ушёл бы в
    busy-loop и выжег остаток лимита за секунду. Значение в 40+ минут (так сервер
    отвечает на исчерпанный часовой лимит) означало бы «спи почти час» — этого мы
    тоже не делаем. Возвращаем 0, если ждать бессмысленно долго: вызывающий сдастся.
    """
    raw = response.headers.get("Retry-After", "")
    try:
        seconds = int(raw)
    except ValueError:
        seconds = 0

    if seconds <= 0:
        return RETRY_DELAY
    return seconds if seconds <= MAX_RETRY_DELAY else 0


def verify_email(email: str) -> dict:
    """Проверка одного адреса: GET /api/EmailValidation?email=...

    Письмо на адрес НЕ отправляется: провайдер обращается к почтовому серверу домена
    и выясняет, принял бы тот письмо на этот ящик.
    """
    for attempt in range(MAX_RETRIES + 1):
        response = requests.get(
            f"{BASE_URL}/api/EmailValidation",
            params={"email": email},
            headers={
                "Authorization": f"Bearer {API_KEY}",
                "Accept": "application/json",
            },
            timeout=TIMEOUT,
        )

        # 429 — не поломка, а реальный лимит продукта. Ждём и повторяем.
        if response.status_code == 429 and attempt < MAX_RETRIES:
            delay = retry_after(response)
            if delay == 0:
                # Сервер просит ждать дольше потолка — значит, исчерпан часовой лимит.
                # Спать 40 минут бессмысленно: честно говорим об этом и выходим.
                raise AtloriumError(429, "лимит запросов по IP исчерпан, повторите позже")
            print(f"  ... лимит запросов, пауза {delay} с", file=sys.stderr)
            time.sleep(delay)
            continue

        if not response.ok:
            raise AtloriumError(response.status_code, response.text)
        return response.json()

    raise AtloriumError(429, "лимит запросов не отпустил после повторов")


# ── Применение данных: чистка списка рассылки ─────────────────────────────────
# Отчёт по адресу сам по себе — просто JSON. Ценность появляется, когда по нему
# принимают решение: слать письмо или нет. Ниже — ровно это решение.

SEND = "SEND"                # отправляем
SEND_RISKY = "SEND_RISKY"    # можно отправить, но на свой риск
RETRY_LATER = "RETRY_LATER"  # проверить не удалось, повторить позже
DROP = "DROP"                # из списка выбросить


@dataclass
class Decision:
    email: str
    status_raw: str
    action: str
    reason: str
    # Спам-ловушка вынесена в отдельный флаг: это не «ещё один DROP»,
    # а причина остановить рассылку и разобраться, откуда взялся адрес.
    spamtrap: bool = False
    # Подсказка по опечатке в домене: gmial.com → gmail.com.
    did_you_mean: str | None = None


def decide(report: dict) -> Decision:
    """Что делать с адресом, исходя из вердикта проверки."""
    status = report.get("status")
    sub = report.get("subStatus")

    if status == "Valid":
        action, reason = SEND, "Ящик существует, домен принимает почту"

    elif status == "CatchAll":
        # Домен отвечает «принято» на ЛЮБОЙ адрес, поэтому существование конкретного
        # ящика проверить снаружи невозможно. Это не «плохо» — это неопределённость.
        # Слать можно, но отдельным сегментом и следя за bounce rate.
        action, reason = SEND_RISKY, "Домен принимает всё подряд — доставляемость не гарантирована"

    elif status == "Unknown":
        # Сервер домена применил greylisting, не ответил или заблокировал проверку.
        # За такой исход деньги НЕ списываются — адрес просто проверяем позже.
        action, reason = RETRY_LATER, "Сервер домена не ответил (greylisting) — запрос не тарифицирован"

    elif status == "Invalid":
        # Ящика не существует. Отправка = hard bounce, а bounce rate выше 2 %
        # почтовые провайдеры считают признаком грязной базы.
        action, reason = DROP, "Ящика не существует — письмо отскочит (hard bounce)"

    elif status == "DoNotMail":
        # Технически ящик может работать, но рассылать на него нельзя.
        if sub == "role_based":
            reason = "Ролевой адрес (info@, sales@) — читает не человек, а отдел"
        elif sub == "disposable":
            reason = "Одноразовый ящик — создан на 10 минут, писать бессмысленно"
        else:
            reason = "Адрес из списка «не писать»"
        action = DROP

    elif status == "Abuse":
        # Владелец адреса уже жаловался на спам. Ещё одна жалоба — минус к репутации.
        action, reason = DROP, "Жалобщик: ранее отмечал письма как спам"

    elif status == "SpamTrap":
        # САМОЕ ВАЖНОЕ, ЧТО ДАЁТ СЕРВИС.
        # Спам-ловушка — адрес, который никто не заводил и на который никто не
        # подписывался: его специально публикуют, чтобы поймать тех, кто рассылает
        # по купленным и спарсенным базам. Ящик отвечает как живой, поэтому отличить
        # его самому невозможно. Одно попадание — и репутация домена-отправителя
        # падает, а в спам уходит ВСЯ рассылка, включая письма живым клиентам.
        return Decision(
            email=report["email"],
            status_raw=report.get("statusRaw", "spamtrap"),
            action=DROP,
            reason="СПАМ-ЛОВУШКА — убивает репутацию домена-отправителя",
            spamtrap=True,
            did_you_mean=report.get("didYouMean"),
        )

    else:
        # Провайдер вернул статус, которого мы не знаем. Не рискуем.
        action, reason = DROP, f"Неизвестный статус: {report.get('statusRaw')}"

    return Decision(
        email=report["email"],
        status_raw=report.get("statusRaw", ""),
        action=action,
        reason=reason,
        did_you_mean=report.get("didYouMean"),
    )


@dataclass
class Summary:
    decisions: list[Decision] = field(default_factory=list)

    def count(self, action: str) -> int:
        return sum(1 for d in self.decisions if d.action == action)

    @property
    def invalid(self) -> int:
        return sum(1 for d in self.decisions if d.status_raw == "invalid")

    @property
    def spamtraps(self) -> list[Decision]:
        return [d for d in self.decisions if d.spamtrap]

    @property
    def typos(self) -> list[Decision]:
        return [d for d in self.decisions if d.did_you_mean]

    @property
    def bounce_rate(self) -> float:
        """Какой был бы bounce rate, если бы список отправили как есть."""
        return 100.0 * self.invalid / len(self.decisions) if self.decisions else 0.0


def filter_mailing_list(emails: list[str]) -> Summary:
    """Чистка списка рассылки: каждый адрес проверяется и получает решение."""
    summary = Summary()
    for email in emails:
        summary.decisions.append(decide(verify_email(email)))
    return summary


def main() -> int:
    if API_KEY == SANDBOX_KEY:
        print("Демо-ключ: ответы сгенерированы (моки), не реальные данные.\n")

    # Список короткий намеренно: проверка почты — платный сервис с жёстким лимитом.
    # Домены ниже — сценарии песочницы, см. README.
    default_list = [
        "anna.petrova@example.com",     # обычный живой ящик
        "ivan@invalid.example.com",     # ящика не существует
        "promo@spamtrap.example.com",   # спам-ловушка
        "pavel@gmial.com",              # опечатка в домене: gmial → gmail
    ]
    emails = [e.strip() for e in sys.argv[1].split(",") if e.strip()] if len(sys.argv) > 1 else default_list

    print(f"Чистка списка рассылки. Адресов на входе: {len(emails)}\n")

    try:
        summary = filter_mailing_list(emails)
    except AtloriumError as error:
        print(f"Ошибка: {error}", file=sys.stderr)
        return 1

    print(f"{'АДРЕС':<32}{'СТАТУС':<11}{'РЕШЕНИЕ':<13}КОММЕНТАРИЙ")
    for d in summary.decisions:
        print(f"{d.email:<32}{d.status_raw:<11}{d.action:<13}{d.reason}")

    if summary.typos:
        print("\nПОДСКАЗКИ ПО ОПЕЧАТКАМ (это спасённый контакт, а не потерянный):")
        for d in summary.typos:
            print(f"  [~] возможно, опечатка: {d.email} → {d.did_you_mean}")

    if summary.spamtraps:
        print(f"\n!!! СПАМ-ЛОВУШКА В СПИСКЕ: {len(summary.spamtraps)} !!!")
        for d in summary.spamtraps:
            print(f"  {d.email}")
        print("  Такой адрес никто не заводил и ни на что не подписывал — его публикуют,")
        print("  чтобы ловить рассылки по купленным базам. Одно попадание роняет репутацию")
        print("  домена-отправителя, после чего в спам уходит ВСЯ рассылка, включая письма")
        print("  живым клиентам. Удалять безусловно и выяснять, откуда адрес взялся.")

    print("\nИТОГО")
    print(f"  Проверено адресов:      {len(summary.decisions)}")
    print(f"  К отправке (SEND):      {summary.count(SEND)}")
    print(f"  На свой риск (RISKY):   {summary.count(SEND_RISKY)}")
    print(f"  Повторить позже:        {summary.count(RETRY_LATER)}  (не тарифицируется)")
    print(f"  Выброшено (DROP):       {summary.count(DROP)}")
    print(f"  Bounce rate без чистки: {summary.bounce_rate:.1f} %  "
          f"(несуществующих ящиков: {summary.invalid} из {len(summary.decisions)})")
    if summary.spamtraps:
        print(f"  Спам-ловушек:           {len(summary.spamtraps)}  <-- критично")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())

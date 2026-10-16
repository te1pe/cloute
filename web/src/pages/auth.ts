// Login and registration.

import { ApiError, api } from '../api';
import { el, mount } from '../ui';

export function renderAuth(root: HTMLElement, note: string, onDone: () => void): void {
  let mode: 'login' | 'register' = 'login';

  const email = el('input', { type: 'email', required: true, autocomplete: 'email', placeholder: 'e-mail' });
  const name = el('input', { required: true, autocomplete: 'name', placeholder: 'Имя', maxLength: 100 });
  const password = el('input', {
    type: 'password',
    required: true,
    minLength: 8,
    maxLength: 72,
    placeholder: 'Пароль (от 8 символов)',
  });
  const error = el('p', { class: 'error' });
  const submit = el('button', { type: 'submit' });
  const toggle = el('button', { type: 'button', class: 'link' });
  const title = el('h1');

  const update = () => {
    const login = mode === 'login';
    title.textContent = login ? 'Вход' : 'Регистрация';
    submit.textContent = login ? 'Войти' : 'Зарегистрироваться';
    toggle.textContent = login ? 'Нет аккаунта? Регистрация' : 'Уже есть аккаунт? Вход';
    name.hidden = login;
    name.required = !login;
    password.autocomplete = login ? 'current-password' : 'new-password';
    error.textContent = '';
  };
  toggle.onclick = () => {
    mode = mode === 'login' ? 'register' : 'login';
    update();
  };

  const form = el('form', { class: 'card' }, title, note && el('p', {}, note), email, name, password, error, submit, toggle);
  form.onsubmit = async (ev) => {
    ev.preventDefault();
    submit.disabled = true;
    try {
      if (mode === 'login') {
        await api.login(email.value, password.value);
      } else {
        await api.register(email.value, name.value, password.value);
      }
      onDone();
    } catch (err) {
      error.textContent = err instanceof ApiError ? describe(err) : 'Сервер недоступен';
      submit.disabled = false;
    }
  };

  update();
  mount(root, form);
}

function describe(err: ApiError): string {
  switch (err.code) {
    case 'invalid_credentials':
      return 'Неверный e-mail или пароль';
    case 'email_taken':
      return 'Этот e-mail уже зарегистрирован';
    default:
      return err.message;
  }
}

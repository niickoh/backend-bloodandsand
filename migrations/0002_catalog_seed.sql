-- Initial catalog. Editable later from the admin panel.

INSERT INTO disciplines (slug, name, tagline, short, description, duration_min, price, image, uses_credit, sort) VALUES
('box', 'Box', 'Despierta tu guerrero', 'Técnica, saco y sparring controlado.',
 'Golpes, defensa y desplazamiento en rounds de 3 minutos. Guantes de préstamo incluidos.', 60, 8000, 'box', TRUE, 1),
('fuerza', 'Fuerza', '+ Fuerte que ayer', 'Barra, mancuernas y progresión semanal.',
 'Básicos de fuerza en grupos de máximo 10 personas, con carga ajustada a tu nivel.', 60, 8000, 'fuerza', TRUE, 2),
('funcional', 'Funcional', 'No + excusas', 'Circuitos de alta intensidad.',
 'Intervalos con kettlebells, cuerdas y peso corporal. Sales sin aire y con ganas de volver.', 45, 7000, 'funcional', TRUE, 3),
('evaluacion', 'Evaluación', 'Plan a tu nivel', 'Gratis. Medimos y armamos tu plan.',
 'Sesión 1 a 1 con un coach: composición corporal, movilidad y test de fuerza. Sales con un plan a tu nivel.', 40, 0, 'hero', FALSE, 4);

INSERT INTO plans (slug, name, price, period_days, credits, booking_window_days, perks, featured, sellable, sort) VALUES
('clase', 'Clase suelta', 8000, 1, 1, 14, ARRAY['Cualquier disciplina', 'Pagas al reservar', 'Sin matrícula'], FALSE, FALSE, 1),
('ocho', 'Plan 8', 39990, 30, 8, 7, ARRAY['8 clases al mes', 'Reserva hasta 7 días antes', 'Evaluación mensual incluida'], FALSE, TRUE, 2),
('ilimitado', 'Sin límites', 54990, 30, NULL, 14,
 ARRAY['Clases ilimitadas', 'Reserva hasta 14 días antes', 'Plan de entrenamiento personalizado', 'Evaluación mensual incluida'], TRUE, TRUE, 3);
